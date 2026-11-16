// 中文说明：安全的本地 Markdown；禁止原始 HTML、命令链接和自动加载远程图片。
import MarkdownIt from 'markdown-it';
const renderer = new MarkdownIt({ html: false, linkify: false, breaks: true, typographer: false });
const baseValidate = renderer.validateLink.bind(renderer);
renderer.validateLink = (url: string) => /^(https?:|mailto:|#)/i.test(url.trim()) && baseValidate(url);
renderer.renderer.rules.image = (tokens, index) => {
  const token = tokens[index];
  return `<span class="reader-image-reference">[图片：${renderer.utils.escapeHtml(token.content || '未命名')}，未自动加载]</span>`;
};
const openLink = renderer.renderer.rules.link_open;
renderer.renderer.rules.link_open = (tokens, index, options, env, self) => {
  tokens[index].attrSet('target', '_blank'); tokens[index].attrSet('rel', 'noopener noreferrer');
  return openLink ? openLink(tokens, index, options, env, self) : self.renderToken(tokens, index, options);
};
renderer.renderer.rules.table_open = () => '<div class="reader-table-scroll"><table>';
renderer.renderer.rules.table_close = () => '</table></div>';
export function renderReaderMarkdown(text: string): string { return renderer.render(text); }
export async function highlightReaderCode(root: HTMLElement, cancelled: () => boolean): Promise<void> {
  // 只有用户展开正文才下载语法模块；显式限制单个代码块，失败时保留原文。
  const blocks = Array.from(root.querySelectorAll<HTMLElement>('pre code')).filter(block => block.textContent && block.textContent.length < 65536);
  if (!blocks.length) return;
  try {
    const { codeToHtml, bundledLanguages } = await import('shiki/bundle/web');
    for (const block of blocks.slice(0, 20)) {
      if (cancelled()) return;
      const language = Array.from(block.classList).find(name => name.startsWith('language-'))?.slice(9) || 'text';
      const lang = language in bundledLanguages ? language as keyof typeof bundledLanguages : 'text';
      const html = await codeToHtml(block.textContent || '', { lang, themes: { light: 'github-light', dark: 'github-dark' } });
      if (cancelled() || !block.isConnected) return;
      const holder = document.createElement('div'); holder.innerHTML = html;
      const highlighted = holder.querySelector('code');
      if (highlighted) block.replaceChildren(...Array.from(highlighted.childNodes));
    }
  } catch { /* 高亮不可用时，代码仍以安全纯文本展示。 */ }
}

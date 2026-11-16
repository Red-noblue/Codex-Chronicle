// 中文说明：声明 Go 与 macOS Foundation 回收站 API 之间的 C 调用边界。
#ifndef CODEX_CHRONICLE_NATIVE_TRASH_DARWIN_H
#define CODEX_CHRONICLE_NATIVE_TRASH_DARWIN_H

int cc_trash_directory(const char *item_path, char **trash_path, char **error_message);
int cc_trash_directory_readonly(const char *item_path, char **trash_path, char **error_message);
int cc_trash_item(const char *source_path, char **trash_path, char **error_message);

#endif

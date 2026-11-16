// 中文说明：统一暴露原生系统回收站入口；平台细节由带构建标签的文件实现。
package platform

// NewNativeSystemTrash 创建当前平台的原生系统回收站 provider。
// Windows 使用 Shell，macOS 使用 Foundation，其他 Unix 平台复用文件系统回收站语义。
func NewNativeSystemTrash() (TrashProvider, error) {
	return newNativeSystemTrash()
}

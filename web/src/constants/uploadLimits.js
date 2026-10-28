// 工作区上传与聊天附件的上限。工作区数字必须与 api.MaxUserUploadBytes 一致。

// 会话工作区与 Bot 文件面板的单文件上限（1000 MiB）。
// 上传是流式的，不再受工具写文件 MaxFileWrite（10 MiB）限制。
// 网关 client_max_body_size 已是 1000m，应用侧与它对齐。
export const MAX_WORKSPACE_UPLOAD_BYTES = 1000 * 1024 * 1024
// 一次选择里待上传文件的合计上限。等于单文件上限，因此一个顶格文件可以通过。
export const MAX_WORKSPACE_UPLOAD_TOTAL_BYTES = MAX_WORKSPACE_UPLOAD_BYTES

// 聊天输入框附件仍走 base64 JSON（POST /api/chat/send），不是工作区流式上传。
// 对应 api.MaxChatAttachmentBytes / MaxChatAttachmentTotalBytes / MaxChatAttachmentCount。
export const MAX_CHAT_ATTACHMENT_BYTES = 20 * 1024 * 1024
export const MAX_CHAT_ATTACHMENT_TOTAL_BYTES = 60 * 1024 * 1024
export const MAX_CHAT_ATTACHMENT_COUNT = 10

export function formatUploadSpeed(bytesPerSec) {
  if (!Number.isFinite(bytesPerSec) || bytesPerSec <= 0) return '…'
  if (bytesPerSec < 1024) return `${Math.round(bytesPerSec)} B/s`
  if (bytesPerSec < 1024 * 1024) return `${(bytesPerSec / 1024).toFixed(1)} KB/s`
  return `${(bytesPerSec / 1024 / 1024).toFixed(1)} MB/s`
}

export function formatUploadPercent(loaded, total) {
  if (!total) return 0
  return Math.max(0, Math.min(100, Math.round((loaded / total) * 100)))
}

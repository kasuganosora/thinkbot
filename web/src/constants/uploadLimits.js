// 工作区上传与聊天附件的上限。工作区数字必须与 api.MaxUserUploadBytes 一致。

// 会话工作区与 Bot 文件面板的单文件上限（1000 MiB）。
// 上传是流式的，不再受工具写文件 MaxFileWrite（10 MiB）限制。
// 网关 client_max_body_size 已是 1000m，应用侧与它对齐。
export const MAX_WORKSPACE_UPLOAD_BYTES = 1000 * 1024 * 1024

// 大于这一段的工作区文件改走分片上传。必须与 api.DefaultChunkUploadBytes 一致（16 MiB）。
// 每一段都远小于常见边缘代理大约 100MB 的单请求体上限。
export const WORKSPACE_UPLOAD_CHUNK_BYTES = 16 * 1024 * 1024
// 浏览器拒绝比这更大的段，避免单次请求又撞上边缘上限。对应 api.MaxChunkUploadBytes。
export const MAX_WORKSPACE_UPLOAD_CHUNK_BYTES = 32 * 1024 * 1024
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

// 不超过一段的文件继续走单次 POST /files/upload。
export function shouldChunkWorkspaceUpload(size) {
  return Number.isFinite(size) && size > WORKSPACE_UPLOAD_CHUNK_BYTES
}

export function workspaceChunkCount(size, chunkSize) {
  if (!Number.isFinite(size) || size <= 0 || !Number.isFinite(chunkSize) || chunkSize <= 0) return 0
  return Math.ceil(size / chunkSize)
}

// 返回第 index 段的半开区间 [start, end)。越界时返回 null。
export function workspaceChunkRange(size, chunkSize, index) {
  if (!Number.isFinite(size) || !Number.isFinite(chunkSize) || chunkSize <= 0 || !Number.isInteger(index) || index < 0) {
    return null
  }
  const count = workspaceChunkCount(size, chunkSize)
  if (index >= count) return null
  const start = index * chunkSize
  const end = Math.min(size, start + chunkSize)
  return { index, start, end, size: end - start }
}

import {
  MAX_WORKSPACE_UPLOAD_CHUNK_BYTES,
  workspaceChunkCount,
  workspaceChunkRange,
} from '../constants/uploadLimits.js'

const CHUNK_PUT_ATTEMPTS = 3

function uploadError(message, code) {
  const err = new Error(message || '上传失败')
  err.code = code || 0
  return err
}

function retriableChunkError(err) {
  const code = Number(err?.code || 0)
  if (!code) return true
  if (code === 408 || code === 429) return true
  return code >= 500
}

async function putChunkWithRetry(put, uploadId, index, blob, onChunk) {
  let last
  for (let attempt = 0; attempt < CHUNK_PUT_ATTEMPTS; attempt++) {
    try {
      return await put(uploadId, index, blob, onChunk)
    } catch (err) {
      last = err
      if (!retriableChunkError(err)) throw err
    }
  }
  throw last
}

/**
 * 按服务端返回的段大小把 file 拆开上传。
 * init/put/complete/abort 由调用方注入，方便测试，也便于沿用现有的 cookie 请求。
 * onProgress 的形状与单次 XHR 上传一致：{ loaded, total, speed }，total 是整个文件。
 */
export async function uploadInChunks({ path, file, onProgress, init, put, complete, abort }) {
  if (!file || !Number.isFinite(file.size) || file.size <= 0) {
    throw uploadError('上传失败', 400)
  }
  let uploadId = ''
  const fail = async (err) => {
    if (uploadId && typeof abort === 'function') {
      try { await abort(uploadId) } catch { /* 清理失败时仍抛原来的错误 */ }
    }
    throw err
  }

  let session
  try {
    session = await init({
      path: path || '/',
      name: file.name,
      size: file.size,
    })
  } catch (err) {
    throw err
  }

  uploadId = session?.uploadId || ''
  const chunkSize = Number(session?.chunkSize)
  const totalChunks = Number(session?.totalChunks)
  if (!uploadId || !Number.isFinite(chunkSize) || chunkSize <= 0 || chunkSize > MAX_WORKSPACE_UPLOAD_CHUNK_BYTES) {
    return fail(uploadError('上传失败', 400))
  }
  if (totalChunks !== workspaceChunkCount(file.size, chunkSize)) {
    return fail(uploadError('上传失败', 400))
  }

  const started = typeof performance !== 'undefined' ? performance.now() : Date.now()
  const now = () => (typeof performance !== 'undefined' ? performance.now() : Date.now())
  let sent = 0
  const report = (chunkLoaded) => {
    if (typeof onProgress !== 'function') return
    const loaded = Math.min(file.size, sent + (Number(chunkLoaded) || 0))
    const seconds = Math.max((now() - started) / 1000, 0.001)
    onProgress({ loaded, total: file.size, speed: loaded / seconds })
  }

  try {
    for (let index = 0; index < totalChunks; index++) {
      const range = workspaceChunkRange(file.size, chunkSize, index)
      if (!range) return fail(uploadError('上传失败', 400))
      const blob = file.slice(range.start, range.end)
      await putChunkWithRetry(put, uploadId, index, blob, (progress) => report(progress?.loaded || 0))
      sent += range.size
      report(0)
    }
    return await complete(uploadId)
  } catch (err) {
    return fail(err)
  }
}

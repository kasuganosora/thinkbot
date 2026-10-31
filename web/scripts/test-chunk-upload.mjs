import assert from 'node:assert/strict'
import test from 'node:test'
import {
  MAX_WORKSPACE_UPLOAD_CHUNK_BYTES,
  WORKSPACE_UPLOAD_CHUNK_BYTES,
  shouldChunkWorkspaceUpload,
  workspaceChunkCount,
  workspaceChunkRange,
} from '../src/constants/uploadLimits.js'
import { uploadInChunks } from '../src/utils/chunkedUpload.js'

test('small files stay on the single request path', () => {
  assert.equal(shouldChunkWorkspaceUpload(0), false)
  assert.equal(shouldChunkWorkspaceUpload(WORKSPACE_UPLOAD_CHUNK_BYTES), false)
  assert.equal(shouldChunkWorkspaceUpload(WORKSPACE_UPLOAD_CHUNK_BYTES + 1), true)
  assert.equal(shouldChunkWorkspaceUpload(Number.NaN), false)
  assert.equal(WORKSPACE_UPLOAD_CHUNK_BYTES, 16 * 1024 * 1024)
  assert.equal(MAX_WORKSPACE_UPLOAD_CHUNK_BYTES, 32 * 1024 * 1024)
})

test('chunk ranges follow the server chunk size', () => {
  assert.equal(workspaceChunkCount(20, 8), 3)
  assert.deepEqual(workspaceChunkRange(20, 8, 0), { index: 0, start: 0, end: 8, size: 8 })
  assert.deepEqual(workspaceChunkRange(20, 8, 1), { index: 1, start: 8, end: 16, size: 8 })
  assert.deepEqual(workspaceChunkRange(20, 8, 2), { index: 2, start: 16, end: 20, size: 4 })
  assert.equal(workspaceChunkRange(20, 8, 3), null)
  assert.equal(workspaceChunkCount(16, 16), 1)
  assert.deepEqual(workspaceChunkRange(16, 16, 0), { index: 0, start: 0, end: 16, size: 16 })
})

function fakeFile(size) {
  return {
    name: 'archive.rar',
    size,
    slice(start, end) {
      return { start, end, size: end - start }
    },
  }
}

test('upload sends slices from the server plan and reports whole-file progress', async () => {
  const puts = []
  const progress = []
  const file = fakeFile(20)
  const result = await uploadInChunks({
    path: '/data/inbox',
    file,
    onProgress: (event) => progress.push(event.loaded),
    init: async (body) => {
      assert.equal(body.name, 'archive.rar')
      assert.equal(body.size, 20)
      assert.equal(body.path, '/data/inbox')
      return { uploadId: 'abc', chunkSize: 8, totalChunks: 3, size: 20 }
    },
    put: async (uploadId, index, blob, onChunk) => {
      puts.push({ uploadId, index, blob })
      onChunk({ loaded: Math.floor(blob.size / 2) })
      onChunk({ loaded: blob.size })
    },
    complete: async (uploadId) => {
      assert.equal(uploadId, 'abc')
      return { ok: true }
    },
    abort: async () => { throw new Error('should not abort') },
  })
  assert.deepEqual(result, { ok: true })
  assert.deepEqual(puts.map(item => item.index), [0, 1, 2])
  assert.deepEqual(puts.map(item => [item.blob.start, item.blob.end]), [[0, 8], [8, 16], [16, 20]])
  assert.equal(progress.at(-1), 20)
  assert.ok(progress.every((loaded, i) => i === 0 || loaded >= progress[i - 1]))
})

test('a dropped chunk is retried, a 400 aborts the session', async () => {
  let puts = 0
  let aborted = 0
  await uploadInChunks({
    path: '/',
    file: fakeFile(8),
    init: async () => ({ uploadId: 'id', chunkSize: 8, totalChunks: 1, size: 8 }),
    put: async (_id, _index, blob, onChunk) => {
      puts++
      if (puts === 1) throw Object.assign(new Error('上传失败'), { code: 0 })
      onChunk({ loaded: blob.size })
    },
    complete: async () => ({ ok: true }),
    abort: async () => { aborted++ },
  })
  assert.equal(puts, 2)
  assert.equal(aborted, 0)

  puts = 0
  aborted = 0
  let completed = 0
  await assert.rejects(() => uploadInChunks({
    path: '/',
    file: fakeFile(8),
    init: async () => ({ uploadId: 'id', chunkSize: 8, totalChunks: 1, size: 8 }),
    put: async () => {
      puts++
      throw Object.assign(new Error('bad chunk'), { code: 400 })
    },
    complete: async () => { completed++ },
    abort: async (uploadId) => {
      assert.equal(uploadId, 'id')
      aborted++
    },
  }), /bad chunk/)
  assert.equal(puts, 1)
  assert.equal(completed, 0)
  assert.equal(aborted, 1)
})

test('rejects a chunk size the edge would refuse', async () => {
  let aborted = 0
  let puts = 0
  await assert.rejects(() => uploadInChunks({
    path: '/',
    file: fakeFile(40),
    init: async () => ({ uploadId: 'id', chunkSize: MAX_WORKSPACE_UPLOAD_CHUNK_BYTES + 1, totalChunks: 1, size: 40 }),
    put: async () => { puts++ },
    complete: async () => ({ ok: true }),
    abort: async () => { aborted++ },
  }))
  assert.equal(puts, 0)
  assert.equal(aborted, 1)
})

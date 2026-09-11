<script setup lang="ts">
import { Plus, UploadFilled } from '@element-plus/icons-vue'
import type { UploadFile } from 'element-plus'
import { ElMessage, ElMessageBox } from 'element-plus'
import { computed, onMounted, reactive, ref, toRef } from 'vue'

import { apiPutRaw } from '@/api/client'
import { imagesApi } from '@/api'
import type { Image } from '@/api/types'
import AppShell from '@/components/AppShell.vue'
import { useQuerySync } from '@/composables/useQuerySync'
import { detectArchFromFilename, detectFromFilename } from '@/lib/filename'
import { fmtAbsolute, fmtBytes } from '@/lib/format'
import { asRow } from '@/lib/typed'

const CHUNK_SIZE = 8 * 1024 * 1024

const list = ref<Image[]>([])
const loading = ref(false)

// 上传状态
const uploadVisible = ref(false)
const file = ref<File | null>(null)
const form = reactive({
  name: '',
  version: '',
  family: '',
  arch: '',
  notes: '',
  expectedSha256: '',
})
const uploadProgress = ref(0)
const uploadPhase = ref<'idle' | 'uploading' | 'finalizing' | 'done'>('idle')
const uploadError = ref('')
/** 失败后保留的会话（续传）：uploaded_chunks 是后端已收块数。 */
const resumeInfo = ref<{ sessionId: string; uploadedChunks: number; totalChunks: number } | null>(null)
let abortUpload = false

// 架构过滤器（与 URL query 同步，便于分享/收藏视图）。
// useQuerySync 接收 reactive 对象；内部用 ref 包一层保持同一代理。
const filters = reactive({ arch: '' as '' | 'amd64' | 'arm64' | 'unknown' })
const archFilter = toRef(filters, 'arch')

useQuerySync(
  filters,
  (st): Record<string, string> => ({ ...(st.arch ? { arch: String(st.arch) } : {}) }),
  (q) => ({ arch: (q.arch as typeof filters.arch) ?? '' }),
)

const filtered = computed(() => {
  if (!archFilter.value) return list.value
  if (archFilter.value === 'unknown') return list.value.filter((i) => !i.arch)
  return list.value.filter((i) => i.arch === archFilter.value)
})

const archCounts = computed(() => ({
  all: list.value.length,
  amd64: list.value.filter((i) => i.arch === 'amd64').length,
  arm64: list.value.filter((i) => i.arch === 'arm64').length,
  unknown: list.value.filter((i) => !i.arch).length,
}))

const knownFamilies = [
  'ubuntu',
  'debian',
  'centos',
  'rocky',
  'almalinux',
  'rhel',
  'kylin',
  'openeuler',
  'opensuse',
]

onMounted(load)

async function load(): Promise<void> {
  loading.value = true
  try {
    list.value = await imagesApi.list()
  } catch (err) {
    ElMessage.error(`加载镜像列表失败: ${(err as Error).message}`)
  } finally {
    loading.value = false
  }
}

function onFileChange(uploadFile: UploadFile): void {
  file.value = uploadFile.raw ?? null
  if (file.value) {
    if (resumeInfo.value) {
      const total = Math.max(1, Math.ceil(file.value.size / CHUNK_SIZE))
      if (resumeInfo.value.totalChunks !== total) resumeInfo.value = null
    }
    if (!form.name) form.name = file.value.name.replace(/\.[^.]+$/, '')
    const guess = detectFromFilename(file.value.name)
    if (guess && !form.family) form.family = guess.family
    // 文件名含架构标记时预填；与后端 detect.go 同规则
    const farch = detectArchFromFilename(file.value.name)
    if (farch && !form.arch) form.arch = farch
  }
}

const canSubmit = computed(
  () =>
    Boolean(file.value) &&
    form.name.trim() !== '' &&
    form.version.trim() !== '' &&
    form.family.trim() !== '' &&
    form.arch !== '',
)

const KNOWN_EXTS = ['.qcow2', '.img', '.raw', '.vmdk', '.qcow', '.vhd', '.vhdx', '.xz', '.gz', '.zst']

/** 文件名扩展名预检：写盘失败发现格式错就太晚了，这里先拦一道。 */
function extWarning(filename: string): string {
  const lower = filename.toLowerCase()
  if (KNOWN_EXTS.some((e) => lower.endsWith(e))) return ''
  return '文件扩展名不在常见磁盘镜像格式中（qcow2/img/raw/vmdk 等），请确认它是可写盘的镜像文件'
}

/** 逐块 SHA-256：后端 X-Chunk-Sha256 头校验，尽早发现传输损坏。 */
async function sha256Hex(buf: ArrayBuffer): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', buf)
  return Array.from(new Uint8Array(digest))
    .map((b) => b.toString(16).padStart(2, '0'))
    .join('')
}

async function startUpload(): Promise<void> {
  if (!file.value || !canSubmit.value) return
  const warn = extWarning(file.value.name)
  if (warn) {
    try {
      await ElMessageBox.confirm(warn + '。仍要继续上传吗？', '格式确认', { type: 'warning' })
    } catch {
      return
    }
  }
  abortUpload = false
  uploadPhase.value = 'uploading'
  uploadProgress.value = 0
  uploadError.value = ''

  let sessionId = ''
  let startChunk = 1
  try {
    const totalChunks = Math.max(1, Math.ceil(file.value.size / CHUNK_SIZE))

    // 1. 会话：失败续传时复用保留的会话（仅当文件大小一致）；否则新建。
    if (resumeInfo.value && resumeInfo.value.totalChunks === totalChunks && file.value.size) {
      sessionId = resumeInfo.value.sessionId
      startChunk = resumeInfo.value.uploadedChunks + 1
      uploadProgress.value = Math.round((resumeInfo.value.uploadedChunks / totalChunks) * 100)
      ElMessage.info(`从第 ${startChunk}/${totalChunks} 块续传`)
    } else {
      const session = await imagesApi.initUpload({
        name: form.name.trim(),
        version: form.version.trim(),
        family: form.family.trim(),
        arch: form.arch,
        notes: form.notes.trim() || undefined,
        expected_sha256: form.expectedSha256.trim() || undefined,
        total_size: file.value.size,
        chunk_size: CHUNK_SIZE,
      })
      sessionId = session.id
      resumeInfo.value = null
    }

    // 2. 分块 PUT（后端编号 1-based；WriteChunk 幂等，重复传同一块安全）。
    //    逐块带 X-Chunk-Sha256，传输损坏当场报错而非等到 finalize。
    for (let n = startChunk; n <= totalChunks; n++) {
      if (abortUpload) throw new Error('已中止')
      const blob = file.value.slice((n - 1) * CHUNK_SIZE, n * CHUNK_SIZE)
      const buf = await blob.arrayBuffer()
      const chunkSha = await sha256Hex(buf)
      const resp = await apiPutRaw(`/images/uploads/${sessionId}/chunks/${n}`, buf, {
        'Content-Type': 'application/octet-stream',
        'X-Chunk-Sha256': chunkSha,
      })
      if (!resp.ok) {
        const text = await resp.text().catch(() => '')
        throw new Error(`块 ${n}/${totalChunks} 上传失败: HTTP ${resp.status} ${text.slice(0, 200)}`)
      }
      uploadProgress.value = Math.round((n / totalChunks) * 100)
    }

    // 3. finalize
    uploadPhase.value = 'finalizing'
    const image = await imagesApi.finalize(sessionId)
    ElMessage.success(`镜像「${image.name}」上传完成`)
    uploadPhase.value = 'done'
    uploadVisible.value = false
    resetForm()
    await load()
  } catch (err) {
    uploadError.value = (err as Error).message
    uploadPhase.value = 'idle'
    // 网络失败保留会话：WriteChunk 幂等，重试同一文件时从 uploaded_chunks
    // 之后继续（前端用文件大小+进度推算已传块数）。仅显式中止时删除会话。
    if (sessionId) {
      if (abortUpload) {
        try {
          await imagesApi.abortUpload(sessionId)
          resumeInfo.value = null
        } catch {
          /* 清理失败只提示主错误 */
        }
      } else {
        try {
          const cur = await imagesApi.getUpload(sessionId)
          resumeInfo.value = {
            sessionId,
            uploadedChunks: cur.uploaded_chunks,
            totalChunks: cur.num_chunks,
          }
        } catch {
          resumeInfo.value = null
        }
      }
    }
  }
}

function resetForm(): void {
  resumeInfo.value = null
  file.value = null
  form.name = ''
  form.version = ''
  form.family = ''
  form.arch = ''
  form.notes = ''
  form.expectedSha256 = ''
  uploadProgress.value = 0
  uploadPhase.value = 'idle'
  uploadError.value = ''
}

async function cancelUpload(): Promise<void> {
  if (uploadPhase.value === 'uploading') {
    try {
      await ElMessageBox.confirm(
        '上传进行中。中止后进度保留，重新选择同一文件可续传。确定中止？',
        '中止上传',
        { type: 'warning' },
      )
    } catch {
      return
    }
    abortUpload = true
  }
  uploadVisible.value = false
  resetForm()
}

async function remove(row: Image): Promise<void> {
  try {
    await ElMessageBox.confirm(
      `删除镜像「${row.name}」将级联删除关联的绑定与作业，确认删除？`,
      '删除镜像',
      { type: 'warning' },
    )
  } catch {
    return
  }
  try {
    await imagesApi.remove(row.id)
    ElMessage.success('已删除')
    await load()
  } catch (err) {
    ElMessage.error(`删除失败: ${(err as Error).message}`)
  }
}
</script>

<template>
  <AppShell>
    <div class="mk-page">
      <header class="mk-page-header">
        <h1 class="mk-page-title">镜像</h1>
        <el-button type="primary" :icon="Plus" @click="uploadVisible = true">上传镜像</el-button>
      </header>

      <el-card class="mk-card">
        <div class="mk-filters">
          <el-radio-group v-model="archFilter">
            <el-radio-button value="">全部 ({{ archCounts.all }})</el-radio-button>
            <el-radio-button value="amd64">x86_64 ({{ archCounts.amd64 }})</el-radio-button>
            <el-radio-button value="arm64">ARM64 ({{ archCounts.arm64 }})</el-radio-button>
            <el-radio-button value="unknown">未知 ({{ archCounts.unknown }})</el-radio-button>
          </el-radio-group>
        </div>
        <el-table v-loading="loading" :data="filtered" stripe>
          <el-table-column prop="name" label="名称" min-width="180" />
          <el-table-column prop="version" label="版本" width="110" />
          <el-table-column prop="family" label="OS 家族" width="110" />
          <el-table-column label="架构" width="80">
            <template #default="{ row }">
              <el-tag v-if="row.arch" :type="row.arch === 'arm64' ? 'warning' : 'info'" size="small" disable-transitions>
                {{ row.arch }}
              </el-tag>
              <span v-else class="mk-subtle">未知</span>
            </template>

          </el-table-column>
          <el-table-column prop="format" label="格式" width="90" />
          <el-table-column label="大小" width="100">
            <template #default="{ row }">{{ fmtBytes(row.size_bytes) }}</template>

          </el-table-column>
          <el-table-column label="SHA-256" min-width="180">
            <template #default="{ row }">
              <span class="mono mk-subtle">{{ row.sha256 ? `${row.sha256.slice(0, 16)}…` : '—' }}</span>
            </template>

          </el-table-column>
          <el-table-column label="上传时间" width="150">
            <template #default="{ row }">{{ fmtAbsolute(row.uploaded_at) }}</template>

          </el-table-column>
          <el-table-column label="操作" width="90" fixed="right">
            <template #default="{ row }">
              <el-button text size="small" type="danger" @click="remove(asRow<Image>(row))">删除</el-button>
            </template>

          </el-table-column>
          <template #empty>
            <el-empty
              :description="archFilter ? '该架构下暂无镜像' : '还没有镜像，点右上角上传'"
              :image-size="72"
            />
          </template>

        </el-table>
      </el-card>

      <el-dialog v-model="uploadVisible" title="上传镜像" width="520px" :close-on-click-modal="false" @close="cancelUpload">
        <el-form label-width="100px">
          <el-form-item label="镜像文件">
            <el-upload
              :auto-upload="false"
              :limit="1"
              :on-change="onFileChange"
              :show-file-list="true"
              drag
              :disabled="uploadPhase === 'uploading' || uploadPhase === 'finalizing'"
            >
              <el-icon :size="36"><UploadFilled /></el-icon>
              <div class="el-upload__text">拖拽文件到此处或 <em>点击选择</em></div>
            </el-upload>
          </el-form-item>
          <el-form-item label="名称">
            <el-input v-model="form.name" placeholder="如 ubuntu-22.04-server" />
          </el-form-item>
          <el-form-item label="版本">
            <el-input v-model="form.version" placeholder="如 22.04.4" />
          </el-form-item>
          <el-form-item label="OS 家族">
            <el-select v-model="form.family" filterable allow-create placeholder="选择或输入">
              <el-option v-for="f in knownFamilies" :key="f" :label="f" :value="f" />
            </el-select>
          </el-form-item>
          <el-form-item label="架构" required>
            <el-radio-group v-model="form.arch">
              <el-radio-button value="amd64">x86_64 (amd64)</el-radio-button>
              <el-radio-button value="arm64">ARM64 (aarch64)</el-radio-button>
            </el-radio-group>
            <div class="mk-subtle">镜像文件名带 amd64/x86_64/arm64/aarch64 时自动预填</div>
          </el-form-item>
          <el-form-item label="SHA-256">
            <el-input v-model="form.expectedSha256" placeholder="可选，64 位十六进制" class="mono" />
            <div class="mk-subtle">留空则跳过完整性校验</div>
          </el-form-item>
          <el-form-item label="备注">
            <el-input v-model="form.notes" type="textarea" :rows="2" placeholder="可选" />
          </el-form-item>
          <el-form-item v-if="uploadPhase === 'uploading' || uploadPhase === 'finalizing'" label="进度">
            <el-progress
              :percentage="uploadProgress"
              :indeterminate="uploadPhase === 'finalizing'"
              :stroke-width="14"
              striped
              striped-flow
            />
          </el-form-item>
          <el-alert v-if="uploadError" type="error" show-icon :closable="false">
            <template #title>{{ uploadError }}</template>

            <div v-if="resumeInfo" class="mk-subtle" style="margin-top: 4px">
              服务器已保留 {{ resumeInfo.uploadedChunks }}/{{ resumeInfo.totalChunks }} 块
              （未改动表单时点击「开始上传」将从下一块续传）
            </div>
          </el-alert>
        </el-form>
        <template #footer>
          <el-button :disabled="uploadPhase === 'uploading' || uploadPhase === 'finalizing'" @click="cancelUpload">
            取消
          </el-button>
          <el-button
            type="primary"
            :disabled="!canSubmit"
            :loading="uploadPhase === 'uploading' || uploadPhase === 'finalizing'"
            @click="startUpload"
          >
            {{ uploadPhase === 'uploading' ? `上传中 ${uploadProgress}%` : '开始上传' }}
          </el-button>
        </template>

      </el-dialog>
    </div>
  </AppShell>
</template>

<style scoped>
.mk-filters {
  display: flex;
  gap: 12px;
  margin-bottom: 14px;
  flex-wrap: wrap;
}
</style>

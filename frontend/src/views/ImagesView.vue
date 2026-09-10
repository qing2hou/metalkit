<script setup lang="ts">
import { Plus, UploadFilled } from '@element-plus/icons-vue'
import type { UploadFile } from 'element-plus'
import { ElMessage, ElMessageBox } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'

import { apiPutRaw } from '@/api/client'
import { imagesApi } from '@/api'
import type { Image } from '@/api/types'
import AppShell from '@/components/AppShell.vue'
import { detectFromFilename } from '@/lib/filename'
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
  notes: '',
  expectedSha256: '',
})
const uploadProgress = ref(0)
const uploadPhase = ref<'idle' | 'uploading' | 'finalizing' | 'done'>('idle')
const uploadError = ref('')
let abortUpload = false

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
    if (!form.name) form.name = file.value.name.replace(/\.[^.]+$/, '')
    const guess = detectFromFilename(file.value.name)
    if (guess && !form.family) form.family = guess.family
  }
}

const canSubmit = computed(
  () =>
    Boolean(file.value) &&
    form.name.trim() !== '' &&
    form.version.trim() !== '' &&
    form.family.trim() !== '',
)

async function startUpload(): Promise<void> {
  if (!file.value || !canSubmit.value) return
  abortUpload = false
  uploadPhase.value = 'uploading'
  uploadProgress.value = 0
  uploadError.value = ''

  let sessionId = ''
  try {
    // 1. 建会话
    const session = await imagesApi.initUpload({
      name: form.name.trim(),
      version: form.version.trim(),
      family: form.family.trim(),
      notes: form.notes.trim() || undefined,
      expected_sha256: form.expectedSha256.trim() || undefined,
      total_size: file.value.size,
      chunk_size: CHUNK_SIZE,
    })
    sessionId = session.id

    // 2. 分块 PUT
    const totalChunks = Math.max(1, Math.ceil(file.value.size / CHUNK_SIZE))
    for (let n = 0; n < totalChunks; n++) {
      if (abortUpload) throw new Error('已中止')
      const blob = file.value.slice(n * CHUNK_SIZE, (n + 1) * CHUNK_SIZE)
      const buf = await blob.arrayBuffer()
      const resp = await apiPutRaw(
        `/images/uploads/${sessionId}/chunks/${n}`,
        buf,
        { 'Content-Type': 'application/octet-stream' },
      )
      if (!resp.ok) {
        const text = await resp.text().catch(() => '')
        throw new Error(`块 ${n} 上传失败: HTTP ${resp.status} ${text.slice(0, 200)}`)
      }
      uploadProgress.value = Math.round(((n + 1) / totalChunks) * 100)
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
    // 失败清理会话，避免服务器残留
    if (sessionId) {
      try {
        await imagesApi.abortUpload(sessionId)
      } catch {
        /* 清理失败只提示主错误 */
      }
    }
  }
}

function resetForm(): void {
  file.value = null
  form.name = ''
  form.version = ''
  form.family = ''
  form.notes = ''
  form.expectedSha256 = ''
  uploadProgress.value = 0
  uploadPhase.value = 'idle'
  uploadError.value = ''
}

async function cancelUpload(): Promise<void> {
  if (uploadPhase.value === 'uploading') {
    try {
      await ElMessageBox.confirm('上传进行中，确定中止？', '中止上传', { type: 'warning' })
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
        <el-table v-loading="loading" :data="list" stripe>
          <el-table-column prop="name" label="名称" min-width="180" />
          <el-table-column prop="version" label="版本" width="110" />
          <el-table-column prop="family" label="OS 家族" width="110" />
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
            <el-empty description="还没有镜像，点右上角上传" :image-size="72" />
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
          <el-alert v-if="uploadError" :title="uploadError" type="error" show-icon :closable="false" />
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

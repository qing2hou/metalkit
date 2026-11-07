<script setup lang="ts">
import type { FormInstance, FormRules } from 'element-plus'
import { usePagedTable } from '@/composables/usePagedTable'
import { ElMessage, ElMessageBox } from 'element-plus'
import { onMounted, reactive, ref } from 'vue'

import { sshKeysApi } from '@/api'
import type { SSHKey } from '@/api/types'
import AppShell from '@/components/AppShell.vue'
import { fmtAbsolute } from '@/lib/format'
import { asRow } from '@/lib/typed'

interface KeyForm {
  id?: string
  name: string
  publicKey: string
}

const list = ref<SSHKey[]>([])
const loading = ref(false)
const dialogVisible = ref(false)
const saving = ref(false)
const formRef = ref<FormInstance>()
const form = reactive<KeyForm>({ name: '', publicKey: '' })

const rules: FormRules = {
  name: [
    { required: true, message: '请输入名称', trigger: 'blur' },
    {
      validator: (_r, v: string, cb) =>
        /^[\p{L}\p{N}][\p{L}\p{N}._-]{0,63}$/u.test(v.trim())
          ? cb()
          : cb(new Error('1-64 字符，字母/数字/文字开头，可含 . _ -')),
      trigger: 'blur',
    },
  ],
  publicKey: [
    { required: true, message: '请粘贴公钥内容', trigger: 'blur' },
    { validator: (_r, v: string, cb) => cb(validatePublicKeyLine(v)), trigger: 'blur' },
  ],
}

// 与后端 sshkeys.ParsePublicKey 对齐的前置校验：单行、2-3 字段、类型白名单、
// base64 可解。指纹/内嵌 type 一致性仍以后端为准（前端只防低级粘贴错误，
// 不复制全部规则——两处白名单漂移时后端是最终权威）。
const knownTypes = new Set([
  'ssh-ed25519',
  'ssh-rsa',
  'ssh-dss',
  'ecdsa-sha2-nistp256',
  'ecdsa-sha2-nistp384',
  'ecdsa-sha2-nistp521',
  'sk-ssh-ed25519@openssh.com',
  'sk-ecdsa-sha2-nistp256@openssh.com',
])

function validatePublicKeyLine(raw: string): Error | undefined {
  const s = raw.trim()
  if (!s) return undefined // required 规则已兜底
  if (s.includes('\n')) return new Error('只支持单行公钥（.pub 文件内容），请勿粘贴私钥或多行文本')
  const fields = s.split(/\s+/)
  if (fields.length < 2) return new Error('缺少 base64 密钥体')
  if (fields.length > 3) return new Error('最多 3 个字段（类型 / base64 / 注释）')
  if (!knownTypes.has(fields[0])) return new Error(`不支持的密钥类型 ${fields[0]}`)
  if (!/^[A-Za-z0-9+/]+={0,3}$/.test(fields[1])) return new Error('base64 密钥体格式无效')
  return undefined
}

onMounted(load)

async function load(): Promise<void> {
  loading.value = true
  try {
    list.value = await sshKeysApi.list()
  } catch (err) {
    ElMessage.error(`加载 SSH 公钥失败: ${(err as Error).message}`)
  } finally {
    loading.value = false
  }
}

function openCreate(): void {
  Object.assign(form, { id: undefined, name: '', publicKey: '' })
  dialogVisible.value = true
}

function openEdit(row: SSHKey): void {
  Object.assign(form, { id: row.id, name: row.name, publicKey: row.public_key })
  dialogVisible.value = true
}

async function save(): Promise<void> {
  const valid = await formRef.value?.validate().catch(() => false)
  if (!valid) return
  saving.value = true
  try {
    const payload = { name: form.name.trim(), public_key: form.publicKey.trim() }
    if (form.id) {
      await sshKeysApi.update(form.id, payload)
      ElMessage.success('SSH 公钥已更新')
    } else {
      await sshKeysApi.create(payload)
      ElMessage.success('SSH 公钥已保存')
    }
    dialogVisible.value = false
    await load()
  } catch (err) {
    ElMessage.error(`保存失败: ${(err as Error).message}`)
  } finally {
    saving.value = false
  }
}

async function remove(row: SSHKey): Promise<void> {
  try {
    await ElMessageBox.confirm(
      `确定删除 SSH 公钥「${row.name}」？正在装机配置中引用它的机器将无法再注入该公钥。`,
      '删除 SSH 公钥',
      { type: 'warning' },
    )
  } catch {
    return
  }
  try {
    await sshKeysApi.remove(row.id)
    ElMessage.success('已删除')
    await load()
  } catch (err) {
    // 422 = 仍被某些机器的装机配置引用
    ElMessage.error(`删除失败: ${(err as Error).message}`)
  }
}

const pagedTable = usePagedTable(list)
</script>

<template>
  <AppShell>
    <div class="mk-page">
      <header class="mk-page-header">
        <h1 class="mk-page-title">SSH 公钥</h1>
        <p class="mk-page-sub">装机时可复用注入的公钥库 — 保存一次，装机时按机器勾选</p>
        <el-button type="primary" @click="openCreate">新建公钥</el-button>
      </header>

      <el-card class="mk-card">
        <el-table class="mk-table" v-loading="loading" :data="pagedTable.paged.value" stripe>
          <el-table-column prop="name" label="名称" min-width="140" />
          <el-table-column prop="key_type" label="类型" min-width="160">
            <template #default="{ row }"><span class="mono">{{ row.key_type }}</span></template>
          </el-table-column>
          <el-table-column prop="fingerprint" label="指纹" min-width="250">
            <template #default="{ row }"><span class="mono">{{ row.fingerprint }}</span></template>
          </el-table-column>
          <el-table-column prop="comment" label="注释" min-width="140">
            <template #default="{ row }">{{ row.comment || '—' }}</template>
          </el-table-column>
          <el-table-column label="创建时间" width="150">
            <template #default="{ row }">{{ fmtAbsolute(row.created_at) }}</template>
          </el-table-column>
          <el-table-column label="操作" min-width="120">
            <template #default="{ row }">
              <el-button text size="small" @click="openEdit(asRow<SSHKey>(row))">编辑</el-button>
              <el-button text size="small" type="danger" @click="remove(asRow<SSHKey>(row))">删除</el-button>
            </template>
          </el-table-column>
          <template #empty>
            <el-empty description="还没有保存的 SSH 公钥" :image-size="72" />
          </template>
        </el-table>
        <el-pagination
          v-if="pagedTable.total.value > pagedTable.pageSize.value"
          v-model:current-page="pagedTable.currentPage.value"
          v-model:page-size="pagedTable.pageSize.value"
          :page-sizes="[20, 50, 100]"
          :total="pagedTable.total.value"
          layout="total, sizes, prev, pager, next, jumper"
          style="margin-top: 16px; justify-content: flex-end"
        />
      </el-card>

      <el-dialog
        v-model="dialogVisible"
        :title="form.id ? '编辑 SSH 公钥' : '新建 SSH 公钥'"
        width="560px"
        destroy-on-close
      >
        <el-form ref="formRef" :model="form" :rules="rules" label-width="90px">
          <el-form-item label="名称" prop="name">
            <el-input v-model="form.name" placeholder="如 ops-main（显示在装机表单的下拉里）" />
          </el-form-item>
          <el-form-item label="公钥" prop="publicKey">
            <el-input
              v-model="form.publicKey"
              type="textarea"
              :rows="3"
              class="mono"
              placeholder="粘贴 ssh-keygen 生成的 .pub 文件内容（一行）"
            />
            <div style="width: 100%; font-size: 12px; color: var(--el-text-color-secondary); line-height: 1.5">
              本地执行 ssh-keygen -t ed25519 生成密钥对后，把 ~/.ssh/id_ed25519.pub 的整行内容粘贴到这里。
              只保存公钥；私钥保留在你自己手里。
            </div>
          </el-form-item>
        </el-form>
        <template #footer>
          <el-button @click="dialogVisible = false">取消</el-button>
          <el-button type="primary" :loading="saving" @click="save">保存</el-button>
        </template>
      </el-dialog>
    </div>
  </AppShell>
</template>

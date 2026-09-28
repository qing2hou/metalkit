<script setup lang="ts">
import { Plus } from '@element-plus/icons-vue'
import type { FormInstance, FormRules } from 'element-plus'
import { ElMessage, ElMessageBox } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'

import { profilesApi, subnetsApi, utilApi } from '@/api'
import type { Profile, ProfileComponents, Subnet } from '@/api/types'
import AppShell from '@/components/AppShell.vue'
import { generatePassword } from '@/lib/filename'
import { fmtAbsolute } from '@/lib/format'
import { isValidIPv4 } from '@/lib/net'
import { asRow } from '@/lib/typed'

const list = ref<Profile[]>([])
const subnets = ref<Subnet[]>([])
const loading = ref(false)

const dialogVisible = ref(false)
const saving = ref(false)
const formRef = ref<FormInstance>()

const form = reactive({
  id: '',
  name: '',
  osFamily: '',
  hostnameTemplate: '',
  // 密码：新填的明文（提交前换 hash）
  rootPassword: '',
  keepExistingPassword: true,
  // 目标盘
  diskMode: 'smallest' as 'smallest' | 'by-wwn' | 'by-path' | 'by-model',
  diskValue: '',
  // 网络
  netMethod: 'dhcp' as 'dhcp' | 'static',
  // 静态配置细节由 binding 覆盖；profile 侧只存默认
  networkRenderer: '',
  bootloader: '',
  // 组件选项
  componentOptions: null as ProfileComponents | null,
})

const rules: FormRules = {
  name: [
    { required: true, message: '请输入名称', trigger: 'blur' },
    { pattern: /^[a-zA-Z0-9][a-zA-Z0-9._-]*$/, message: '字母数字开头，可含 . _ -', trigger: 'blur' },
  ],
  osFamily: [{ required: true, message: '请输入 OS 家族', trigger: 'blur' }],
  hostnameTemplate: [
    { pattern: /^[a-zA-Z0-9{}._-]*$/, message: '仅字母数字、. _ - 与 {var} 占位符', trigger: 'blur' },
  ],
}

onMounted(load)

async function load(): Promise<void> {
  loading.value = true
  try {
    const [ps, subs] = await Promise.all([profilesApi.list(), subnetsApi.list()])
    list.value = ps
    subnets.value = subs
  } catch (err) {
    ElMessage.error(`加载 profiles 失败: ${(err as Error).message}`)
  } finally {
    loading.value = false
  }
}

async function loadComponents(osFamily: string): Promise<void> {
  if (!osFamily) {
    form.componentOptions = null
    return
  }
  try {
    form.componentOptions = await profilesApi.components(osFamily)
  } catch {
    form.componentOptions = null
  }
}

function openCreate(): void {
  Object.assign(form, {
    id: '',
    name: '',
    osFamily: '',
    hostnameTemplate: '',
    rootPassword: '',
    keepExistingPassword: true,
    diskMode: 'smallest',
    diskValue: '',
    netMethod: 'dhcp',
    networkRenderer: '',
    bootloader: '',
    componentOptions: null,
  })
  dialogVisible.value = true
}

function openEdit(row: Profile): void {
  Object.assign(form, {
    id: row.id,
    name: row.name,
    osFamily: row.os_family,
    hostnameTemplate: row.hostname_template ?? '',
    rootPassword: '',
    keepExistingPassword: true,
    diskMode: row.target_disk?.mode ?? 'smallest',
    diskValue: row.target_disk?.value ?? '',
    netMethod: row.network?.method ?? 'dhcp',
    networkRenderer: row.network_renderer ?? '',
    bootloader: row.bootloader ?? '',
    componentOptions: null,
  })
  dialogVisible.value = true
  void loadComponents(row.os_family)
}

const rendererOptions = computed(() => form.componentOptions?.renderers ?? [])
const bootloaderOptions = computed(() => form.componentOptions?.bootloaders ?? [])

async function save(): Promise<void> {
  const valid = await formRef.value?.validate().catch(() => false)
  if (!valid) return
  saving.value = true
  try {
    // 后端约束：by-* 模式必须带 value
    if (form.diskMode !== 'smallest' && !form.diskValue.trim()) {
      ElMessage.warning('by-wwn / by-path / by-model 模式必须填写匹配值')
      saving.value = false
      return
    }

    // 密码：仅在用户填写新明文时提交（先换 hash）
    const passwordPart: Record<string, unknown> = {}
    if (form.rootPassword) {
      const { hash } = await utilApi.cryptSha512(form.rootPassword)
      passwordPart.root_password_hash = hash
    }

    if (form.id) {
      // 更新走 UpdateInput：name 不可改（后端 DisallowUnknownFields 会拒），
      // os_family / network_renderer / bootloader 是三态字段——仅在用户
      // 实际选择时提交，留空（回 auto）显式发 ""。
      await profilesApi.update(form.id, {
        hostname_template: form.hostnameTemplate.trim(),
        target_disk: {
          mode: form.diskMode,
          ...(form.diskMode !== 'smallest' && form.diskValue
            ? { value: form.diskValue.trim() }
            : {}),
        },
        network: { method: form.netMethod, nic_selector: 'auto' },
        network_renderer: form.networkRenderer || '',
        bootloader: form.bootloader || '',
        ...passwordPart,
      })
      ElMessage.success('Profile 已更新')
    } else {
      // 创建走 CreateInput：name / os_family 必填，target_disk / network
      // 必填（smallest 时 value 留空）。
      await profilesApi.create({
        name: form.name.trim(),
        os_family: form.osFamily.trim(),
        hostname_template: form.hostnameTemplate.trim(),
        target_disk: {
          mode: form.diskMode,
          ...(form.diskMode !== 'smallest' && form.diskValue
            ? { value: form.diskValue.trim() }
            : {}),
        },
        network: { method: form.netMethod, nic_selector: 'auto' },
        network_renderer: form.networkRenderer || undefined,
        bootloader: form.bootloader || undefined,
        ...passwordPart,
      })
      ElMessage.success('Profile 已创建')
    }
    dialogVisible.value = false
    await load()
  } catch (err) {
    ElMessage.error(`保存失败: ${(err as Error).message}`)
  } finally {
    saving.value = false
  }
}

async function remove(row: Profile): Promise<void> {
  try {
    await ElMessageBox.confirm(`删除安装配置「${row.name}」？`, '删除', { type: 'warning' })
  } catch {
    return
  }
  try {
    await profilesApi.remove(row.id)
    ElMessage.success('已删除')
    await load()
  } catch (err) {
    // 422 = 被 binding 引用
    ElMessage.error(`删除失败: ${(err as Error).message}`)
  }
}
</script>

<template>
  <AppShell>
    <div class="mk-page">
      <header class="mk-page-header">
        <h1 class="mk-page-title">安装配置（Profiles）</h1>
        <el-button type="primary" :icon="Plus" @click="openCreate">新建 Profile</el-button>
      </header>

      <el-card class="mk-card">
        <el-table v-loading="loading" :data="list" stripe>
          <el-table-column prop="name" label="名称" min-width="160" />
          <el-table-column prop="os_family" label="OS 家族" width="110" />
          <el-table-column label="主机名模板" min-width="180">
            <template #default="{ row }">{{ row.hostname_template || '—' }}</template>
          </el-table-column>
          <el-table-column label="目标盘" min-width="160">
            <template #default="{ row }">
              <span class="mono">{{ row.target_disk?.mode || 'smallest' }}{{ row.target_disk?.value ? ` · ${row.target_disk.value}` : '' }}</span>
            </template>
          </el-table-column>
          <el-table-column label="网络" width="90">
            <template #default="{ row }">{{ row.network?.method || 'dhcp' }}</template>
          </el-table-column>
          <el-table-column label="渲染器 / 引导器" min-width="170">
            <template #default="{ row }">
              {{ row.network_renderer || '默认' }} / {{ row.bootloader || '默认' }}
            </template>
          </el-table-column>
          <el-table-column label="更新时间" width="150">
            <template #default="{ row }">{{ fmtAbsolute(row.updated_at ?? row.created_at) }}</template>
          </el-table-column>
          <el-table-column label="操作" width="140" fixed="right">
            <template #default="{ row }">
              <el-button text size="small" @click="openEdit(asRow<Profile>(row))">编辑</el-button>
              <el-button text size="small" type="danger" @click="remove(asRow<Profile>(row))">删除</el-button>
            </template>
          </el-table-column>
          <template #empty>
            <el-empty description="还没有 profile" :image-size="72" />
          </template>
        </el-table>
      </el-card>

      <el-dialog
        v-model="dialogVisible"
        :title="form.id ? '编辑 Profile' : '新建 Profile'"
        width="640px"
        destroy-on-close
      >
        <el-form ref="formRef" :model="form" :rules="rules" label-width="110px">
          <el-row :gutter="16">
            <el-col :span="12">
              <el-form-item label="名称" prop="name">
                <el-input v-model="form.name" placeholder="如 prod-base" />
              </el-form-item>
            </el-col>
            <el-col :span="12">
              <el-form-item label="OS 家族" prop="osFamily">
                <el-select
                  v-model="form.osFamily"
                  filterable
                  allow-create
                  placeholder="选择或输入"
                  style="width: 100%"
                  @change="loadComponents"
                >
                  <el-option v-for="f in ['ubuntu', 'debian', 'centos', 'rocky', 'almalinux', 'rhel', 'kylin', 'openeuler', 'opensuse']" :key="f" :label="f" :value="f" />
                </el-select>
              </el-form-item>
            </el-col>
          </el-row>

          <el-form-item label="主机名模板">
            <el-input v-model="form.hostnameTemplate" placeholder="如 node-{serial}，支持 {serial}/{mac}/{uuid}" class="mono" />
          </el-form-item>

          <el-form-item label="root 密码">
            <div class="mk-password-row">
              <el-input
                v-model="form.rootPassword"
                type="password"
                show-password
                class="mono"
                :placeholder="form.id ? '留空 = 保留现有 hash' : '必填（提交时转 hash）'"
              />
              <el-button @click="form.rootPassword = generatePassword(24)">🎲</el-button>
            </div>
            <div class="mk-subtle">以 crypt-sha512 hash 存储在服务端；编辑时留空表示保留原密码</div>
          </el-form-item>

          <el-divider content-position="left">目标盘</el-divider>
          <el-row :gutter="16">
            <el-col :span="9">
              <el-form-item label="选择方式">
                <el-select v-model="form.diskMode" style="width: 100%">
                  <el-option label="最小磁盘" value="smallest" />
                  <el-option label="按 WWN" value="by-wwn" />
                  <el-option label="按路径" value="by-path" />
                  <el-option label="按型号" value="by-model" />
                </el-select>
              </el-form-item>
            </el-col>
            <el-col v-if="form.diskMode !== 'smallest'" :span="15">
              <el-form-item label="匹配值">
                <el-input
                  v-model="form.diskValue"
                  class="mono"
                  :placeholder="form.diskMode === 'by-wwn' ? '如 0x5000c50015ea71ad' : form.diskMode === 'by-path' ? '如 /dev/disk/by-path/pci-0000:03:00.0' : '如 INTEL SSDSC2KB480G8'"
                />
              </el-form-item>
            </el-col>
          </el-row>

          <el-divider content-position="left">网络与组件</el-divider>
          <el-row :gutter="16">
            <el-col :span="12">
              <el-form-item label="地址方式">
                <el-radio-group v-model="form.netMethod">
                  <el-radio-button value="dhcp">DHCP</el-radio-button>
                  <el-radio-button value="static">静态</el-radio-button>
                </el-radio-group>
                <div v-if="form.netMethod === 'static'" class="mk-subtle">
                  静态地址在装机弹窗按机器填写（binding 级）
                </div>
              </el-form-item>
            </el-col>
            <el-col :span="12">
              <el-form-item label="子网默认">
                <span class="mk-subtle">在装机弹窗中选择</span>
              </el-form-item>
            </el-col>
          </el-row>
          <el-row :gutter="16">
            <el-col :span="12">
              <el-form-item label="渲染器">
                <el-select v-model="form.networkRenderer" clearable placeholder="默认" style="width: 100%">
                  <el-option v-for="r in rendererOptions" :key="r.id" :label="r.label" :value="r.id" />
                </el-select>
              </el-form-item>
            </el-col>
            <el-col :span="12">
              <el-form-item label="引导器">
                <el-select v-model="form.bootloader" clearable placeholder="默认" style="width: 100%">
                  <el-option v-for="b in bootloaderOptions" :key="b.id" :label="b.label" :value="b.id" />
                </el-select>
              </el-form-item>
            </el-col>
          </el-row>
        </el-form>
        <template #footer>
          <el-button @click="dialogVisible = false">取消</el-button>
          <el-button type="primary" :loading="saving" @click="save">保存</el-button>
        </template>
      </el-dialog>
    </div>
  </AppShell>
</template>

<style scoped>
.mk-password-row {
  display: flex;
  gap: 8px;
  width: 100%;
}
</style>

<script setup lang="ts">
import type { FormInstance, FormRules } from 'element-plus'
import { ElMessage } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'

import { settingsApi } from '@/api'
import type { DhcpSettings, InterfaceInfo } from '@/api/types'
import AppShell from '@/components/AppShell.vue'
import { isValidIPv4, subnetRange } from '@/lib/net'

const loading = ref(false)
const saving = ref(false)
const restartRequired = ref(false)
const savedOnce = ref(false)

const interfaces = ref<InterfaceInfo[]>([])

const form = reactive<DhcpSettings>({
  mode: 'proxy',
  interface: '',
  start: '',
  end: '',
  netmask: '255.255.255.0',
  gateway: '',
  dns: [],
  lease_hours: 24,
  exclude: [],
})

// 输入辅助（逗号/换行分隔 → 数组）
const dnsText = ref('')
const excludeText = ref('')

const netmaskOptions = [
  '255.255.255.0',
  '255.255.254.0',
  '255.255.248.0',
  '255.255.240.0',
  '255.255.0.0',
  '255.0.0.0',
]

const rules: FormRules = {
  interface: [{ required: true, message: '请选择网卡', trigger: 'change' }],
  gateway: [
    { required: true, message: '请输入网关', trigger: 'blur' },
    {
      validator: (_r, v: string, cb) => cb(isValidIPv4(v) ? undefined : new Error('无效 IPv4')),
      trigger: 'blur',
    },
  ],
  start: [
    { required: true, message: '请输入起始 IP', trigger: 'blur' },
    {
      validator: (_r, v: string, cb) => cb(isValidIPv4(v) ? undefined : new Error('无效 IPv4')),
      trigger: 'blur',
    },
    {
      validator: (_r, v: string, cb) => {
        if (!form.end || !isValidIPv4(v) || !isValidIPv4(form.end)) return cb()
        // 起止倒置由后端兜底，这里提前给出友好提示
        const toInt = (ip: string) => ip.split('.').reduce((a, o) => a * 256 + Number(o), 0)
        cb(toInt(v) <= toInt(form.end) ? undefined : new Error('起始 IP 大于结束 IP'))
      },
      trigger: 'blur',
    },
  ],
  end: [
    { required: true, message: '请输入结束 IP', trigger: 'blur' },
    {
      validator: (_r, v: string, cb) => cb(isValidIPv4(v) ? undefined : new Error('无效 IPv4')),
      trigger: 'blur',
    },
  ],
  netmask: [
    { required: true, message: '请选择子网掩码', trigger: 'change' },
    {
      validator: (_r, v: string, cb) => {
        if (!isValidIPv4(v)) return cb(new Error('无效掩码'))
        // 必须是连续掩码：二进制 1 前缀
        const bin = v.split('.').map((o) => Number(o).toString(2).padStart(8, '0')).join('')
        cb(/^1*0*$/.test(bin) ? undefined : new Error('不是合法的连续子网掩码'))
      },
      trigger: 'change',
    },
    {
      // 池必须落在 gateway/netmask 决定的子网内（与后端同规则，提前提示）
      validator: (_r, v: string, cb) => {
        if (!isValidIPv4(v) || !isValidIPv4(form.gateway) || !isValidIPv4(form.start) || !isValidIPv4(form.end)) return cb()
        const toInt = (ip: string) => ip.split('.').reduce((a, o) => a * 256 + Number(o), 0)
        const mask = toInt(v)
        const net = toInt(form.gateway) & mask
        for (const ip of [form.start, form.end]) {
          if ((toInt(ip) & mask) >>> 0 !== net >>> 0) {
            return cb(new Error(`IP ${ip} 不在网关所在子网内`))
          }
        }
        cb()
      },
      trigger: 'blur',
    },
  ],
  lease_hours: [{ required: true, message: '请输入租期', trigger: 'blur' }],
}

const selectedIface = computed(() => interfaces.value.find((i) => i.name === form.interface))
const poolSummary = computed(() => {
  if (form.mode !== 'full' || !isValidIPv4(form.gateway) || !isValidIPv4(form.netmask)) return ''
  const range = subnetRange(`${form.gateway}/${maskToPrefix(form.netmask)}`)
  if (!range) return ''
  return `子网 ${range.network} 的可用主机范围 ${range.broadcast}`
})

function maskToPrefix(mask: string): number {
  const bin = mask.split('.').map((o) => Number(o).toString(2).padStart(8, '0')).join('')
  return bin.lastIndexOf('1') + 1
}

onMounted(load)

async function load(): Promise<void> {
  loading.value = true
  try {
    const [s, ifaces] = await Promise.all([settingsApi.getDhcp(), settingsApi.interfaces()])
    Object.assign(form, {
      mode: s.mode,
      interface: s.interface,
      start: s.start,
      end: s.end,
      netmask: s.netmask || '255.255.255.0',
      gateway: s.gateway,
      dns: s.dns ?? [],
      lease_hours: s.lease_hours || 24,
      exclude: s.exclude ?? [],
    })
    dnsText.value = (s.dns ?? []).join(', ')
    excludeText.value = (s.exclude ?? []).join(', ')
    interfaces.value = ifaces
  } catch (err) {
    ElMessage.error(`加载 DHCP 设置失败: ${(err as Error).message}`)
  } finally {
    loading.value = false
  }
}

async function save(): Promise<void> {
  const valid = await formRef.value?.validate().catch(() => false)
  if (!valid) {
    ElMessage.warning('表单存在校验错误，请修正后再保存')
    return
  }
  saving.value = true
  try {
    // 空的网卡选项回退到"当前生效"值（后端对空值做同样处理）
    const payload: DhcpSettings = {
      ...form,
      interface: form.interface || '',
      dns: dnsText.value
        .split(/[,，\n]/)
        .map((x) => x.trim())
        .filter(Boolean),
      exclude: excludeText.value
        .split(/[,，\n]/)
        .map((x) => x.trim())
        .filter(Boolean),
    }
    const resp = await settingsApi.putDhcp(payload)
    restartRequired.value = Boolean(resp?.restart_required)
    savedOnce.value = true
    ElMessage.success('DHCP 设置已保存')
  } catch (err) {
    ElMessage.error(`保存失败: ${(err as Error).message}`)
  } finally {
    saving.value = false
  }
}

const formRef = ref<FormInstance>()
</script>

<template>
  <AppShell>
    <div class="mk-page">
      <header class="mk-page-header">
        <h1 class="mk-page-title">设置 · DHCP</h1>
      </header>

      <el-card v-loading="loading" class="mk-card">
        <el-form
          ref="formRef"
          :model="form"
          :rules="rules"
          label-width="130px"
          style="max-width: 760px"
        >
          <el-form-item label="模式" prop="mode">
            <el-radio-group v-model="form.mode">
              <el-radio-button value="proxy">Proxy（旁路）</el-radio-button>
              <el-radio-button value="full">Full（全量）</el-radio-button>
            </el-radio-group>
            <div class="mk-subtle">
              proxy：只对 PXE 客户端应答引导信息，地址由网段内其他 DHCP 服务器分配；
              full：metalkit 自己分配地址（下方地址池生效）。
            </div>
          </el-form-item>

          <el-form-item label="绑定网卡" prop="interface">
            <el-select
              v-model="form.interface"
              filterable
              placeholder="选择 DHCP 监听的网卡"
              style="width: 100%"
            >
              <el-option
                v-for="i in interfaces"
                :key="i.name"
                :label="`${i.name}${i.ipv4 ? ` (${i.ipv4})` : ' (无 IPv4)'}${i.up ? '' : ' · DOWN'}`"
                :value="i.name"
                :disabled="!i.up"
              />
            </el-select>
            <div v-if="selectedIface" class="mk-subtle">
              {{ selectedIface.ipv4 ? `当前地址 ${selectedIface.ipv4}` : '该网卡无 IPv4 地址' }}
              <template v-if="selectedIface.is_current">· 当前生效</template>
              — 变更网卡需重启 controller 后生效
            </div>
          </el-form-item>

          <template v-if="form.mode === 'full'">
            <el-divider content-position="left">地址池</el-divider>

            <el-row :gutter="16">
              <el-col :span="10">
                <el-form-item label="子网掩码" prop="netmask">
                  <el-select v-model="form.netmask" allow-create filterable style="width: 100%">
                    <el-option v-for="m in netmaskOptions" :key="m" :label="m" :value="m" />
                  </el-select>
                </el-form-item>
              </el-col>
              <el-col :span="10">
                <el-form-item label="网关" prop="gateway">
                  <el-input v-model="form.gateway" placeholder="192.168.10.1" class="mono" />
                </el-form-item>
              </el-col>
            </el-row>

            <el-row :gutter="16">
              <el-col :span="10">
                <el-form-item label="起始 IP" prop="start">
                  <el-input v-model="form.start" placeholder="192.168.10.100" class="mono" />
                </el-form-item>
              </el-col>
              <el-col :span="10">
                <el-form-item label="结束 IP" prop="end">
                  <el-input v-model="form.end" placeholder="192.168.10.200" class="mono" />
                </el-form-item>
              </el-col>
            </el-row>

            <div v-if="poolSummary" class="mk-subtle" style="margin: -8px 0 12px 130px">
              {{ poolSummary }}
            </div>

            <el-form-item label="租期（小时）" prop="lease_hours">
              <el-input-number v-model="form.lease_hours" :min="1" :max="720" />
            </el-form-item>

            <el-form-item label="DNS">
              <el-input v-model="dnsText" placeholder="如 223.5.5.5, 8.8.8.8（逗号分隔）" class="mono" />
              <div class="mk-subtle">留空则使用默认（8.8.8.8 / 1.1.1.1）。随租约下发给客户端。</div>
            </el-form-item>

            <el-form-item label="排除 IP">
              <el-input v-model="excludeText" placeholder="如 192.168.10.50, 192.168.10.99（逗号分隔）" class="mono" />
              <div class="mk-subtle">
                池内不分配的地址（预留给静态设备）。controller IP 与网关始终自动排除。
              </div>
            </el-form-item>
          </template>
        </el-form>

        <div style="margin-left: 130px">
          <el-button type="primary" :loading="saving" @click="save">保存</el-button>
        </div>
      </el-card>

      <el-alert
        v-if="restartRequired"
        title="已保存。DHCP 网段等设置已热重载，但绑定网卡的变更需要重启 controller 才能完全生效"
        type="warning"
        show-icon
        :closable="true"
        style="margin-top: 16px"
      />
      <el-alert
        v-else-if="savedOnce && form.mode === 'full'"
        title="已保存并热重载生效（网段/网关/DNS/租期立即生效，无需重启）"
        type="success"
        show-icon
        :closable="true"
        style="margin-top: 16px"
      />
    </div>
  </AppShell>
</template>

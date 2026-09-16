<script setup lang="ts">
import { ElMessage } from 'element-plus'
import { computed, reactive, ref, watch } from 'vue'

import { bindingsApi } from '@/api'
import type { Binding, Image, Profile, Report, Subnet } from '@/api/types'
import { generatePassword } from '@/lib/filename'
import { hostInSubnet, isValidIPv4 } from '@/lib/net'
import { fmtBytes } from '@/lib/format'

const props = defineProps<{
  modelValue: boolean
  machineUuid: string
  report: Report | null
  images: Image[]
  profiles: Profile[]
  subnets: Subnet[]
  binding: Binding | null
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', v: boolean): void
  (e: 'saved'): void
}>()

const visible = computed({
  get: () => props.modelValue,
  set: (v) => emit('update:modelValue', v),
})

// ---------- 表单状态 ----------

const form = reactive({
  imageId: '',
  profileId: '',
  desiredState: 'install' as 'install' | 'reinstall',
  subnetId: '',
  ipMode: 'auto' as 'auto' | 'static',
  staticIp: '',
  rootPassword: '',
  usePasswordOverride: false,
  // 目标盘（三态，来自 profile 默认值，可覆盖）
  diskMode: 'smallest' as 'smallest' | 'by-wwn' | 'by-path' | 'by-model',
  diskValue: '',
  // NIC
  nicMode: 'auto' as 'auto' | 'mac',
  nicMac: '',
  // Bond
  useBond: false,
  bondMode: 'active-backup',
  bondSlaves: [] as string[],
  bondMiimon: 100,
  bondLacpRate: '',
  bondXmitHashPolicy: '',
  bondPrimary: '',
})

const saving = ref(false)
const disks = computed(() => props.report?.disks ?? [])
const nics = computed(() => props.report?.nics ?? [])

// 机器上报的 CPU 架构（Report.cpu.arch；未上报为空 → 不做前端判断，后端兜底）
const machineArch = computed(() => props.report?.cpu?.arch ?? '')

const selectedProfile = computed(() => props.profiles.find((p) => p.id === form.profileId))

// profile 的网络方式决定"自动/静态"的含义：static profile 下两者都走子网分配
const profileUsesStatic = computed(() => selectedProfile.value?.network?.method === 'static')

// profile → 联动默认值（对应旧版 profile→subnet 联动）
watch(selectedProfile, (p) => {
  if (!p) return
  if (p.target_disk) {
    form.diskMode = (['smallest', 'by-wwn', 'by-path', 'by-model'] as const).includes(
      p.target_disk.mode as 'smallest',
    )
      ? (p.target_disk.mode as 'smallest' | 'by-wwn' | 'by-path' | 'by-model')
      : 'smallest'
    form.diskValue = p.target_disk.value ?? ''
  }
})

// 打开时用现有 binding 初始化
watch(
  () => props.modelValue,
  (open) => {
    if (!open) return
    form.imageId = props.binding?.image_id ?? ''
    form.profileId = props.binding?.profile_id ?? ''
    form.desiredState = (props.binding?.desired_state as 'install') ?? 'install'
    form.subnetId = props.binding?.subnet_id ?? ''
    if (props.binding?.static_address) {
      form.ipMode = 'static'
      form.staticIp = props.binding.static_address
    } else {
      form.ipMode = 'auto'
      form.staticIp = ''
    }
    form.usePasswordOverride = false
    form.rootPassword = ''
    form.nicMode = 'auto'
    form.nicMac = ''
    // binding 只覆盖 ip/subnet；bond 配置来自 profile，弹窗内可再覆盖
    const pbond = selectedProfile.value?.network?.bond ?? null
    form.useBond = Boolean(pbond)
    form.bondMode = pbond?.mode ?? 'active-backup'
    form.bondSlaves = pbond?.slaves ?? []
    form.bondMiimon = pbond?.miimon ?? 100
    form.bondLacpRate = pbond?.lacp_rate ?? ''
    form.bondXmitHashPolicy = pbond?.xmit_hash_policy ?? ''
    form.bondPrimary = pbond?.primary ?? ''
  },
)

// ---------- 校验 ----------

const staticIpError = computed(() => {
  if (form.ipMode !== 'static' || !form.staticIp) return ''
  if (!isValidIPv4(form.staticIp)) return '静态 IP 不是合法 IPv4'
  const subnet = props.subnets.find((s) => s.id === form.subnetId)
  if (subnet && !hostInSubnet(form.staticIp, subnet.cidr)) {
    return `IP 不在子网 ${subnet.cidr} 内`
  }
  return ''
})

// 装机密码：勾选覆盖后若填写则需 8-128 字符（后端 bindings.validatePassword 同规则）
const passwordError = computed(() => {
  if (!form.usePasswordOverride) return ''
  const p = form.rootPassword
  if (p === '') return '' // 留空 = 沿用 profile / 已存密码
  if (p.length < 8) return '密码至少 8 个字符'
  if (p.length > 128) return '密码最长 128 个字符'
  return ''
})

const canSubmit = computed(
  () =>
    form.imageId !== '' &&
    form.profileId !== '' &&
    (form.ipMode !== 'static' || !staticIpError.value) &&
    !passwordError.value &&
    (form.diskMode === 'smallest' || form.diskValue !== '') &&
    (form.nicMode === 'auto' || form.nicMac !== '') &&
    (!form.useBond || form.bondSlaves.length >= 2),
)

// ---------- 密码 ----------

async function randomizePassword(): Promise<void> {
  form.rootPassword = generatePassword(24)
}

async function fetchManagedPassword(): Promise<void> {
  try {
    const res = await bindingsApi.password(props.machineUuid)
    form.rootPassword = res.password
    form.usePasswordOverride = false
    ElMessage.info('已取出当前绑定的装机密码')
  } catch (err) {
    ElMessage.error(`读取密码失败: ${(err as Error).message}`)
  }
}

// ---------- 提交 ----------

async function submit(): Promise<void> {
  if (!canSubmit.value) {
    ElMessage.warning('请先补全表单中的必填/校验项')
    return
  }
  // 前端架构防呆；未知侧（机器没上报/镜像旧无 arch）放行，后端 ErrArchMismatch 兜底
  const img = props.images.find((i) => i.id === form.imageId)
  if (machineArch.value && img?.arch && img.arch !== machineArch.value) {
    ElMessage.error(`镜像架构 ${img.arch} 与机器架构 ${machineArch.value} 不符`)
    return
  }
  saving.value = true
  try {
    // 密码覆盖：binding.root_password 接收明文（8-128 字符），服务端 AES 加密
    // 存储、装机时才转换为 $6$ hash 写入目标系统（见 jobs/agent_api.go）。
    // 依赖 HTTPS 传输（server 已支持 TLS）。

    // binding 覆盖字段（三态语义见 bindings.UpsertInput）：
    //   target_disk / bond 键存在且为 "null" 字符串 = 清除；键不传 = 沿用 profile
    //   注意此处发送的是 bindings 的顶层字段，非 profiles.network
    const payload: Record<string, unknown> = {
      image_id: form.imageId,
      profile_id: form.profileId,
      desired_state: form.desiredState,
      // subnet_id：留空选择时显式发 "" 清除，否则发所选子网
      subnet_id: form.subnetId || '',
      ...(form.ipMode === 'static' && form.staticIp ? { static_address: form.staticIp } : {}),
      ...(form.usePasswordOverride && form.rootPassword
        ? { root_password: form.rootPassword }
        : {}),
      // target_disk：非 smallest 或带匹配值时才覆盖
      ...(form.diskMode !== 'smallest' || form.diskValue
        ? {
            target_disk: {
              mode: form.diskMode,
              ...(form.diskMode !== 'smallest' ? { value: form.diskValue } : {}),
            },
          }
        : {}),
      // NIC：by-mac 覆盖；auto 显式清空（回退 profile 默认）
      nic_selector_override: form.nicMode === 'mac' ? `by-mac:${form.nicMac}` : '',
      // bond 永远显式发送（与旧版一致）：null = 清除绑定级 bond
      bond: form.useBond
        ? {
            mode: form.bondMode,
            slaves: form.bondSlaves,
            miimon: form.bondMiimon,
            ...(form.bondLacpRate ? { lacp_rate: form.bondLacpRate } : {}),
            ...(form.bondXmitHashPolicy ? { xmit_hash_policy: form.bondXmitHashPolicy } : {}),
            ...(form.bondPrimary ? { primary: form.bondPrimary } : {}),
          }
        : 'null',
    }

    await bindingsApi.upsert(props.machineUuid, payload)
    ElMessage.success('装机配置已保存，协调器将自动创建作业')
    visible.value = false
    emit('saved')
  } catch (err) {
    ElMessage.error(`保存失败: ${(err as Error).message}`)
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <el-dialog v-model="visible" title="配置装机" width="720px" top="6vh" destroy-on-close>
    <el-form label-width="120px">
      <el-row :gutter="16">
        <el-col :span="12">
          <el-form-item label="镜像" required>
            <el-select v-model="form.imageId" filterable placeholder="选择镜像" style="width: 100%">
              <el-option
                v-for="img in images"
                :key="img.id"
                :label="`${img.name} ${img.version} (${img.family}${img.arch ? ', ' + img.arch : ''})`"
                :value="img.id"
                :disabled="Boolean(machineArch && img.arch && img.arch !== machineArch)"
              />
            </el-select>
            <div v-if="machineArch" class="mk-subtle">本机架构 {{ machineArch }} — 架构不符的镜像已禁用</div>
          </el-form-item>
        </el-col>
        <el-col :span="12">
          <el-form-item label="安装配置" required>
            <el-select
              v-model="form.profileId"
              filterable
              placeholder="选择 profile"
              style="width: 100%"
            >
              <el-option
                v-for="p in profiles"
                :key="p.id"
                :label="p.name"
                :value="p.id"
              />
            </el-select>
          </el-form-item>
        </el-col>
      </el-row>

      <el-row :gutter="16">
        <el-col :span="12">
          <el-form-item label="期望状态">
            <el-radio-group v-model="form.desiredState">
              <el-radio-button value="install">install（首次）</el-radio-button>
              <el-radio-button value="reinstall">reinstall</el-radio-button>
            </el-radio-group>
          </el-form-item>
        </el-col>
        <el-col :span="12">
          <el-form-item label="子网">
            <el-select v-model="form.subnetId" clearable placeholder="留空 = DHCP" style="width: 100%">
              <el-option v-for="s in subnets" :key="s.id" :label="`${s.name} (${s.cidr})`" :value="s.id" />
            </el-select>
          </el-form-item>
        </el-col>
      </el-row>

      <el-divider content-position="left">网络</el-divider>

      <el-row :gutter="16">
        <el-col :span="8">
          <el-form-item label="地址分配">
            <el-radio-group v-model="form.ipMode">
              <el-radio-button value="auto">自动</el-radio-button>
              <el-radio-button value="static">静态</el-radio-button>
            </el-radio-group>
          </el-form-item>
        </el-col>
        <el-col v-if="form.ipMode === 'static'" :span="8">
          <el-form-item label="静态 IP" :error="staticIpError || undefined">
            <el-input v-model="form.staticIp" placeholder="留空 = 自动分配" class="mono" />
          </el-form-item>
        </el-col>
        <el-col :span="8">
          <el-form-item label=" ">
            <span class="mk-subtle">
              {{
                profileUsesStatic
                  ? form.subnetId
                    ? form.ipMode === 'static' && form.staticIp
                      ? '使用指定地址'
                      : '留空/自动均由所选子网分配空闲 IP（先做 ARP 存活探测，避开已用地址）'
                    : 'profile 为静态方式，需选择子网才能自动分配 IP；也可直接填入地址'
                  : form.subnetId
                    ? 'profile 为 DHCP 方式：本机由 DHCP 取址，子网仅提供网关/DNS/VLAN'
                    : 'profile 为 DHCP 方式：由 DHCP 取址'
              }}
            </span>
          </el-form-item>
        </el-col>
      </el-row>

      <el-row :gutter="16">
        <el-col :span="12">
          <el-form-item label="引导网卡">
            <el-radio-group v-model="form.nicMode">
              <el-radio-button value="auto">自动</el-radio-button>
              <el-radio-button value="mac">按 MAC</el-radio-button>
            </el-radio-group>
          </el-form-item>
        </el-col>
        <el-col v-if="form.nicMode === 'mac'" :span="12">
          <el-form-item label="选择网卡" required>
            <el-select v-model="form.nicMac" placeholder="按 MAC 选择" style="width: 100%">
              <el-option
                v-for="n in nics"
                :key="n.mac"
                :label="`${n.name} (${n.mac}${n.link ? '' : ' · DOWN'})`"
                :value="n.mac"
              />
            </el-select>
          </el-form-item>
        </el-col>
      </el-row>

      <el-form-item label="Bond">
        <el-switch v-model="form.useBond" />
      </el-form-item>

      <template v-if="form.useBond">
        <el-row :gutter="16">
          <el-col :span="8">
            <el-form-item label="模式">
              <el-select v-model="form.bondMode" style="width: 100%">
                <el-option
                  v-for="m in ['active-backup', '802.3ad', 'balance-rr', 'balance-xor', 'broadcast', 'balance-tlb', 'balance-alb']"
                  :key="m"
                  :label="m"
                  :value="m"
                />
              </el-select>
            </el-form-item>
          </el-col>
          <el-col :span="8">
            <el-form-item label="miimon (ms)">
              <el-input-number v-model="form.bondMiimon" :min="0" :step="50" />
            </el-form-item>
          </el-col>
          <el-col :span="8">
            <el-form-item label="primary">
              <el-select v-model="form.bondPrimary" clearable placeholder="可选" style="width: 100%">
                <el-option v-for="n in nics" :key="n.mac" :label="n.name" :value="n.name" />
              </el-select>
            </el-form-item>
          </el-col>
        </el-row>
        <el-row v-if="form.bondMode === '802.3ad'" :gutter="16">
          <el-col :span="12">
            <el-form-item label="LACP rate">
              <el-select v-model="form.bondLacpRate" clearable style="width: 100%">
                <el-option label="slow (0)" value="slow" />
                <el-option label="fast (1)" value="fast" />
              </el-select>
            </el-form-item>
          </el-col>
          <el-col :span="12">
            <el-form-item label="xmit_hash_policy">
              <el-select v-model="form.bondXmitHashPolicy" clearable style="width: 100%">
                <el-option v-for="p in ['layer2', 'layer2+3', 'layer3+4', 'encap2+3', 'encap3+4']" :key="p" :label="p" :value="p" />
              </el-select>
            </el-form-item>
          </el-col>
        </el-row>
        <el-form-item label="成员口" required>
          <el-select v-model="form.bondSlaves" multiple style="width: 100%" placeholder="至少选择 2 个">
            <el-option
              v-for="n in nics"
              :key="n.mac"
              :label="`${n.name} (${n.mac})`"
              :value="n.name"
            />
          </el-select>
        </el-form-item>
      </template>

      <el-divider content-position="left">目标盘</el-divider>

      <el-row :gutter="16">
        <el-col :span="8">
          <el-form-item label="选择方式">
            <el-radio-group v-model="form.diskMode">
              <el-radio-button value="smallest">最小</el-radio-button>
              <el-radio-button value="by-wwn">WWN</el-radio-button>
              <el-radio-button value="by-path">路径</el-radio-button>
              <el-radio-button value="by-model">型号</el-radio-button>
            </el-radio-group>
          </el-form-item>
        </el-col>
        <el-col v-if="form.diskMode !== 'smallest'" :span="16">
          <el-form-item label="目标盘" required>
            <el-select
              v-if="form.diskMode === 'by-wwn'"
              v-model="form.diskValue"
              filterable
              style="width: 100%"
              placeholder="选择磁盘 WWN"
            >
              <el-option
                v-for="d in disks"
                :key="d.wwn ?? d.path"
                :label="`${d.kname} · ${fmtBytes(d.size_bytes)} · ${d.wwn || '(无 WWN)'}`"
                :value="d.wwn ?? ''"
                :disabled="!d.wwn"
              />
            </el-select>
            <el-select
              v-else-if="form.diskMode === 'by-path'"
              v-model="form.diskValue"
              filterable
              style="width: 100%"
              placeholder="选择磁盘路径"
            >
              <el-option
                v-for="d in disks"
                :key="d.path"
                :label="`${d.path} · ${fmtBytes(d.size_bytes)}`"
                :value="d.path"
              />
            </el-select>
            <el-select
              v-else
              v-model="form.diskValue"
              filterable
              allow-create
              style="width: 100%"
              placeholder="选择或输入磁盘型号"
            >
              <el-option
                v-for="d in disks"
                :key="d.model ?? d.kname"
                :label="`${d.model || '(无型号)'} · ${fmtBytes(d.size_bytes)}`"
                :value="d.model ?? ''"
                :disabled="!d.model"
              />
            </el-select>
          </el-form-item>
        </el-col>
      </el-row>

      <el-divider content-position="left">root 密码</el-divider>

      <el-form-item label="覆盖 profile">
        <el-switch v-model="form.usePasswordOverride" />
        <span class="mk-subtle" style="margin-left: 8px">
          {{ selectedProfile ? '不勾选则使用 profile 内的密码 hash' : '' }}
        </span>
      </el-form-item>
      <el-form-item v-if="form.usePasswordOverride" label="装机密码" :error="passwordError || undefined">
        <div class="mk-password-row">
          <el-input
            v-model="form.rootPassword"
            class="mono"
            type="password"
            show-password
            placeholder="输入自定义密码（8-128 字符；留空 = 沿用 profile）"
          />
          <el-button @click="randomizePassword">🎲 随机</el-button>
        </div>
        <div class="mk-subtle">本次装机 root 密码；加密存储在服务端，装机时转换为 shadow hash</div>
      </el-form-item>
      <el-form-item v-else-if="binding" label=" ">
        <el-button text size="small" @click="fetchManagedPassword">查看当前绑定密码</el-button>
      </el-form-item>
    </el-form>

    <template #footer>
      <el-button @click="visible = false">取消</el-button>
      <el-button type="primary" :loading="saving" :disabled="!canSubmit" @click="submit">
        保存并发装
      </el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.mk-password-row {
  display: flex;
  gap: 8px;
  width: 100%;
}
</style>

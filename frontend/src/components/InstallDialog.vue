<script setup lang="ts">
import { ElMessage, ElMessageBox } from 'element-plus'
import { computed, reactive, ref, watch } from 'vue'

import { bindingsApi } from '@/api'
import type { Binding, Image, Profile, Report, Subnet } from '@/api/types'
import { generatePassword } from '@/lib/filename'
import { hostInSubnet, isValidIPv4 } from '@/lib/net'
import { fmtBytes } from '@/lib/format'

const props = defineProps<{
  modelValue: boolean
  machineUuid: string
  /** 'reinstall'：由机器页「立即重装」进入——预填当前配置、隐藏期望状态选择，
   *  底栏变成「确认重装并执行」，提交前走两步确认。 */
  intent?: 'install' | 'reinstall' 
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

const isReinstall = computed(() => props.intent === 'reinstall')

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
  // 监控 agent 植入（三态：跟随 profile / 强制开 / 强制关）
  agentMode: 'inherit' as 'inherit' | 'on' | 'off',
})

const saving = ref(false)
const disks = computed(() => props.report?.disks ?? [])
const nics = computed(() => props.report?.nics ?? [])

// 机器上报的 CPU 架构（Report.cpu.arch；未上报为空 → 不做前端判断，后端兜底）
const machineArch = computed(() => props.report?.cpu?.arch ?? '')

const selectedProfile = computed(() => props.profiles.find((p) => p.id === form.profileId))

// profile 的网络方式决定地址分配口径：
//   static → 静态（留空 = 从子网探测可用地址后自动分配）
//   dhcp   → 由 DHCP 取址，绝不分配静态地址
const profileUsesStatic = computed(() => selectedProfile.value?.network?.method === 'static')

// profile → 联动默认值（用户手动切换 profile 时）。弹窗打开时的预填
// 会赋 form.profileId 并间接触发本 watch——此时不能让 profile 默认值
// 覆盖刚回填的上次装机配置，用 prefilling 标志跳过这一拍。
let prefilling = false
watch(selectedProfile, (p, old) => {
  if (!p) return
  if (prefilling || old === undefined) return
  // 地址方式跟随 profile：静态 profile 默认静态（留空自动分配），
  // DHCP profile 固定 DHCP；切换 profile 时清掉不属于该方式的输入。
  if (p.network?.method === 'dhcp') {
    form.ipMode = 'auto'
    form.staticIp = ''
  } else {
    form.ipMode = 'static'
  }
  if (p.target_disk) {
    form.diskMode = (['smallest', 'by-wwn', 'by-path', 'by-model'] as const).includes(
      p.target_disk.mode as 'smallest',
    )
      ? (p.target_disk.mode as 'smallest' | 'by-wwn' | 'by-path' | 'by-model')
      : 'smallest'
    form.diskValue = p.target_disk.value ?? ''
  }
})

// 打开时用现有 binding 初始化：binding 里存的就是「上次装机的配置」
// （image/profile/子网/静态 IP/目标盘/NIC/bond 都是装机时下发并被
// Upsert 持久化的），全部预填回来，用户在拷贝上改即可，不用重选。
watch(
  () => props.modelValue,
  (open) => {
    if (!open) return
    prefilling = true
    const b = props.binding
    form.imageId = b?.image_id ?? ''
    form.profileId = b?.profile_id ?? ''
    form.desiredState = isReinstall.value ? 'reinstall' : ((b?.desired_state as 'install') ?? 'install')
    form.subnetId = b?.subnet_id ?? ''
    if (b?.static_address) {
      form.ipMode = 'static'
      form.staticIp = b.static_address
    } else if (b?.subnet_id && selectedProfile.value?.network?.method === 'static') {
      // 静态 profile：留空即"自动分配"，交给后端探测挑地址
      form.ipMode = 'static'
      form.staticIp = ''
    } else {
      form.ipMode = 'auto'
      form.staticIp = ''
    }
    form.usePasswordOverride = false
    form.rootPassword = ''
    // 目标盘：上次的覆盖值优先；无覆盖（smallest）回退 profile 默认
    if (b?.target_disk) {
      form.diskMode = (['smallest', 'by-wwn', 'by-path', 'by-model'] as const).includes(
        b.target_disk.mode as 'smallest',
      )
        ? (b.target_disk.mode as 'smallest' | 'by-wwn' | 'by-path' | 'by-model')
        : 'smallest'
      form.diskValue = b.target_disk.value ?? ''
    } else {
      form.diskMode = selectedProfile.value?.target_disk?.mode as typeof form.diskMode ?? 'smallest'
      form.diskValue = selectedProfile.value?.target_disk?.value ?? ''
    }
    // NIC：上次 by-mac 覆盖则带回
    const nicSel = b?.nic_selector_override ?? ''
    if (nicSel.startsWith('by-mac:')) {
      form.nicMode = 'mac'
      form.nicMac = nicSel.slice('by-mac:'.length)
    } else {
      form.nicMode = 'auto'
      form.nicMac = ''
    }
    // Bond：上次 binding 级覆盖优先，无覆盖回退 profile 默认
    const bond = b?.bond ?? selectedProfile.value?.network?.bond ?? null
    form.useBond = Boolean(bond)
    form.bondMode = bond?.mode ?? 'active-backup'
    form.bondSlaves = bond?.slaves ?? []
    form.bondMiimon = bond?.miimon ?? 100
    form.bondLacpRate = bond?.lacp_rate ?? ''
    form.bondXmitHashPolicy = bond?.xmit_hash_policy ?? ''
    form.bondPrimary = bond?.primary ?? ''
    // 监控 agent：binding 覆盖优先（true/false），否则跟随 profile
    if (typeof b?.agent_installed_override === 'boolean') {
      form.agentMode = b.agent_installed_override ? 'on' : 'off'
    } else {
      form.agentMode = selectedProfile.value?.agent_installed ? 'on' : 'inherit'
    }
    // 等这一拍 selectedProfile watch flush 后再放开联动（flush:'post' 的
    // watch 在 nextTick 前后触发，setTimeout 0 足够排在其后）。
    setTimeout(() => {
      prefilling = false
    }, 0)
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

// ---------- 重装确认（两步） ----------

async function confirmReinstall(): Promise<void> {
  if (!canSubmit.value) {
    ElMessage.warning('请先补全表单中的必填/校验项')
    return
  }
  const img = props.images.find((i) => i.id === form.imageId)
  const prof = props.profiles.find((pr) => pr.id === form.profileId)
  const disk = form.diskMode === 'smallest' ? '最小磁盘' : `${form.diskMode}=${form.diskValue}`
  try {
    await ElMessageBox.confirm(
      `即将擦除机器 ${props.machineUuid.slice(0, 13)} 的磁盘并重新写入系统` +
        `（镜像 ${img ? img.name : form.imageId.slice(0, 8)}，Profile ${prof ? prof.name : form.profileId.slice(0, 8)}，目标盘 ${disk}）。` +
        '磁盘数据将全部丢失，此操作不可恢复。',
      '重装确认',
      { type: 'warning', confirmButtonText: '继续', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    const { value } = await ElMessageBox.prompt('请输入「重装」两个字以确认执行', '最终确认', {
      confirmButtonText: '执行重装',
      cancelButtonText: '取消',
      inputValidator: (v: string) => v.trim() === '重装' || '输入不正确，请输入「重装」',
    })
    if (value.trim() !== '重装') return
  } catch {
    return
  }
  await submit(true)
}

// ---------- 提交 ----------

// dispatch=true：保存并触发装机（desired_state 用表单选择的 install/reinstall）。
// dispatch=false：仅保存配置（desired_state=none），编排器不会建作业——
// 改镜像/密码/磁盘等参数但不想立刻擦盘重装时用这个。
async function submit(dispatch: boolean): Promise<void> {
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
    //   target_disk / bond 发 JSON null（或 {}/""）= 清除；键不传 = 沿用 profile。
    //   注意给的是 JSON null 而不是字符串 'null'：后者会被后端当成类型错误拒绝。
    //   注意此处发送的是 bindings 的顶层字段，非 profiles.network
    const payload: Record<string, unknown> = {
      image_id: form.imageId,
      profile_id: form.profileId,
      desired_state: dispatch ? form.desiredState : 'none',
      // subnet_id：留空选择时显式发 "" 清除，否则发所选子网
      subnet_id: form.subnetId || '',
      // 静态 + 填了 IP：用该地址；静态 + 留空：后端探测后自动分配。
      // 非静态（DHCP）：显式清空，避免残留旧静态地址。
      ...(form.ipMode === 'static'
        ? form.staticIp
          ? { static_address: form.staticIp }
          : {}
        : { static_address: '' }),
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
      // 监控 agent 植入：覆盖值（true/false）改写本机行为；inherit 清除覆盖（回 profile）
      ...(form.agentMode !== 'inherit'
        ? { agent_installed_override: form.agentMode === 'on' }
        : { agent_installed_override: null }),
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
        : null,
    }

    await bindingsApi.upsert(props.machineUuid, payload)
    ElMessage.success(
      dispatch
        ? isReinstall.value
          ? '重装已触发：作业已创建，机器将通过 BMC 以 PXE 重启'
          : '装机配置已保存，协调器将自动创建作业'
        : '配置已保存（未触发装机；需要时在机器页点「立即重装」）',
    )
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
  <el-dialog v-model="visible" :title="isReinstall ? '重装机器' : '配置装机'" width="720px" top="6vh" destroy-on-close>
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
          <el-form-item v-if="!isReinstall" label="期望状态">
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
              <el-radio-button value="auto" :disabled="profileUsesStatic">DHCP</el-radio-button>
              <el-radio-button value="static" :disabled="!profileUsesStatic && !!form.profileId">
                静态
              </el-radio-button>
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
                !form.profileId
                  ? '先选择安装配置，地址方式随后自动匹配'
                  : profileUsesStatic
                    ? form.ipMode === 'static'
                      ? form.staticIp
                        ? '使用指定地址'
                        : form.subnetId
                          ? '留空：从所选子网探测空闲地址后自动分配'
                          : '留空需先选择子网；也可直接填入地址'
                      : ''
                    : 'profile 为 DHCP 方式：本机由 DHCP 取址，不分配静态地址'
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

      <el-divider content-position="left">监控 Agent</el-divider>

      <el-form-item label="植入监控">
        <el-radio-group v-model="form.agentMode">
          <el-radio-button value="inherit">
            跟随 Profile{{ selectedProfile ? `（${selectedProfile.agent_installed ? '开启' : '关闭'}）` : '' }}
          </el-radio-button>
          <el-radio-button value="on">本次植入</el-radio-button>
          <el-radio-button value="off">本次不植入</el-radio-button>
        </el-radio-group>
        <div class="mk-subtle" style="width: 100%">
          植入后系统内运行独立的监控 agent，仅收集 CPU / 内存 / 磁盘 / 网络指标并周期上报到本平台
        </div>
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
              allow-create
              style="width: 100%"
              placeholder="选择或输入磁盘路径"
            >
              <el-option
                v-for="d in disks"
                :key="d.path"
                :label="`${d.by_path || d.path} · ${fmtBytes(d.size_bytes)}`"
                :value="d.by_path || d.path"
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
      <el-button :loading="saving" :disabled="!canSubmit" @click="submit(false)">
        仅保存配置
      </el-button>
      <el-button
        v-if="isReinstall"
        type="danger"
        :loading="saving"
        :disabled="!canSubmit"
        @click="confirmReinstall"
      >
        确认重装并执行
      </el-button>
      <el-button v-else type="primary" :loading="saving" :disabled="!canSubmit" @click="submit(true)">
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

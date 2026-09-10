<script setup lang="ts">
import { ElMessage } from 'element-plus'
import { onMounted, reactive, ref } from 'vue'

import { settingsApi } from '@/api'
import type { DhcpSettings } from '@/api/types'
import AppShell from '@/components/AppShell.vue'

const loading = ref(false)
const saving = ref(false)
const restartRequired = ref(false)

const form = reactive<DhcpSettings>({})

onMounted(async () => {
  loading.value = true
  try {
    const s = await settingsApi.getDhcp()
    Object.assign(form, s)
  } catch (err) {
    ElMessage.error(`加载 DHCP 设置失败: ${(err as Error).message}`)
  } finally {
    loading.value = false
  }
})

async function save(): Promise<void> {
  saving.value = true
  try {
    const resp = await settingsApi.putDhcp({ ...form })
    restartRequired.value = Boolean(resp?.restart_required)
    ElMessage.success('DHCP 设置已保存')
  } catch (err) {
    ElMessage.error(`保存失败: ${(err as Error).message}`)
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <AppShell>
    <div class="mk-page">
      <header class="mk-page-header">
        <h1 class="mk-page-title">设置 · DHCP</h1>
      </header>

      <el-card v-loading="loading" class="mk-card">
        <el-form label-width="140px" style="max-width: 640px">
          <el-form-item label="模式">
            <el-radio-group v-model="form.mode">
              <el-radio-button value="proxy">Proxy（旁路）</el-radio-button>
              <el-radio-button value="full">Full（全量）</el-radio-button>
            </el-radio-group>
            <div class="mk-subtle">
              proxy 模式只响应 PXE 客户端；full 模式提供完整 DHCP 服务（含地址分配）。
            </div>
          </el-form-item>
          <el-form-item label="监听接口">
            <el-input v-model="form.interface" placeholder="如 eth0" class="mono" />
          </el-form-item>
          <el-form-item>
            <el-button type="primary" :loading="saving" @click="save">保存</el-button>
          </el-form-item>
        </el-form>
      </el-card>

      <el-alert
        v-if="restartRequired"
        title="设置已热重载，但部分变更需要重启 controller 才能完全生效"
        type="warning"
        show-icon
        :closable="false"
        style="margin-top: 16px"
      />
    </div>
  </AppShell>
</template>

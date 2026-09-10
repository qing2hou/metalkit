<script setup lang="ts">
import { computed } from 'vue'

import { statusLabel, statusTagType } from '@/lib/format'

const props = defineProps<{
  status?: string | null
  /** 未知状态时是否原样显示值 */
  raw?: boolean
}>()

const label = computed(() => {
  if (props.raw && props.status && statusLabel(props.status) === props.status) return props.status
  return statusLabel(props.status)
})
const type = computed(() => statusTagType(props.status ?? ''))
</script>

<template>
  <el-tag :type="type" size="small" disable-transitions>{{ label }}</el-tag>
</template>

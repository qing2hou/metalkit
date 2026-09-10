<script setup lang="ts">
import { CopyDocument } from '@element-plus/icons-vue'
import { ElMessage } from 'element-plus'
import { computed } from 'vue'

const props = defineProps<{
  text: string
  /** 显示文本（默认同 text） */
  label?: string
}>()

const display = computed(() => props.label ?? props.text)

async function copy(): Promise<void> {
  try {
    await navigator.clipboard.writeText(props.text)
    ElMessage.success('已复制')
  } catch {
    ElMessage.warning('复制失败，请手动选择复制')
  }
}
</script>

<template>
  <span class="mk-copyable mono" :title="`点击复制：${text}`" @click="copy">
    <span class="mk-copyable-text">{{ display }}</span>
    <el-icon :size="12"><CopyDocument /></el-icon>
  </span>
</template>

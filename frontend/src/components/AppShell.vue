<script setup lang="ts">
import { Moon, Sunny } from '@element-plus/icons-vue'
import { useDark } from '@vueuse/core'
import { computed, onMounted } from 'vue'
import { useRoute } from 'vue-router'

import { useAuthStore } from '@/stores/auth'

// 暗色模式跟随系统（与旧版 prefers-color-scheme 行为一致），切换时同步 html.dark
const isDark = useDark()
const themeIcon = computed(() => (isDark.value ? Sunny : Moon))
onMounted(() => {
  useAuthStore().fetchMe()
})

const route = useRoute()
const auth = useAuthStore()

interface NavItem {
  to: string
  label: string
  matchPrefix?: string
}

const navItems: NavItem[] = [
  { to: '/', label: '机器' },
  { to: '/images', label: '镜像' },
  { to: '/profiles', label: '安装配置' },
  { to: '/subnets', label: '子网' },
  { to: '/bmc', label: 'BMC' },
  { to: '/jobs', label: '作业' },
  { to: '/settings', label: '设置' },
]

function isActive(item: NavItem): boolean {
  if (item.to === '/') return route.path === '/'
  return route.path === item.to || route.path.startsWith(item.to + '/')
}
</script>

<template>
  <el-container class="mk-shell">
    <el-header class="mk-topbar" height="56px">
      <div class="mk-topbar-inner">
        <router-link to="/" class="mk-brand"> metalkit </router-link>
        <nav class="mk-nav">
          <router-link
            v-for="item in navItems"
            :key="item.to"
            :to="item.to"
            class="mk-nav-link"
            :class="{ active: isActive(item) }"
          >
            {{ item.label }}
          </router-link>
        </nav>
        <div class="mk-topbar-right">
          <el-tooltip :content="isDark ? '切换亮色' : '切换暗色'">
            <el-button text circle aria-label="切换主题" @click="isDark = !isDark">
              <el-icon><component :is="themeIcon" /></el-icon>
            </el-button>
          </el-tooltip>
          <span v-if="auth.username" class="mk-user">{{ auth.username }}</span>
          <el-button v-if="auth.username" text size="small" @click="auth.logout()">退出</el-button>
        </div>
      </div>
    </el-header>
    <el-main class="mk-main">
      <slot />
    </el-main>
  </el-container>
</template>

<style scoped>
.mk-shell {
  min-height: 100vh;
}

.mk-topbar {
  background: var(--el-bg-color);
  border-bottom: 1px solid var(--el-border-color-light);
  position: sticky;
  top: 0;
  z-index: 100;
}

.mk-topbar-inner {
  max-width: 1240px;
  margin: 0 auto;
  height: 100%;
  display: flex;
  align-items: center;
  gap: 28px;
  padding: 0 24px;
}

.mk-brand {
  font-weight: 700;
  font-size: 16px;
  letter-spacing: 0.02em;
  color: var(--el-text-color-primary);
  text-decoration: none;
}

.mk-nav {
  display: flex;
  gap: 4px;
  flex: 1;
}

.mk-nav-link {
  padding: 6px 12px;
  border-radius: 6px;
  color: var(--el-text-color-regular);
  text-decoration: none;
  font-size: 14px;
}

.mk-nav-link:hover {
  color: var(--el-color-primary);
  background: var(--el-fill-color-light);
}

.mk-nav-link.active {
  color: var(--el-color-primary);
  background: var(--el-color-primary-light-9);
  font-weight: 500;
}

.mk-topbar-right {
  display: flex;
  align-items: center;
  gap: 10px;
}

.mk-user {
  color: var(--el-text-color-secondary);
  font-size: 13px;
}
</style>

import { createRouter, createWebHistory } from 'vue-router'

import { useAuthStore } from '@/stores/auth'

const router = createRouter({
  // 服务端把 /ui/ 及其子路径都回退到 index.html（SPA fallback），
  // Vue Router 接管 /ui/ 前缀下的全部导航。
  history: createWebHistory('/ui/'),
  routes: [
    {
      path: '/login',
      name: 'login',
      component: () => import('@/views/LoginView.vue'),
      meta: { public: true },
    },
    { path: '/', name: 'machines', component: () => import('@/views/MachinesView.vue') },
    { path: '/m/:uuid', name: 'machine-detail', component: () => import('@/views/MachineDetailView.vue') },
    { path: '/images', name: 'images', component: () => import('@/views/ImagesView.vue') },
    { path: '/profiles', name: 'profiles', component: () => import('@/views/ProfilesView.vue') },
    { path: '/subnets', name: 'subnets', component: () => import('@/views/SubnetsView.vue') },
    { path: '/bmc', name: 'bmc', component: () => import('@/views/BmcView.vue') },
    { path: '/jobs', name: 'jobs', component: () => import('@/views/JobsView.vue') },
    { path: '/jobs/:id', name: 'job-detail', component: () => import('@/views/JobDetailView.vue') },
    { path: '/settings', name: 'settings', component: () => import('@/views/SettingsView.vue') },
    { path: '/audit', name: 'audit', component: () => import('@/views/AuditView.vue') },
    { path: '/:pathMatch(.*)*', name: 'not-found', redirect: '/' },
  ],
})

router.beforeEach(async (to) => {
  const auth = useAuthStore()
  if (!auth.loaded) {
    await auth.fetchMe()
  }
  if (to.meta.public) {
    // 已登录用户访问登录页直接进首页
    if (auth.username && to.path === '/login') return { path: '/' }
    return true
  }
  if (!auth.username) {
    // next 只保留站内非登录路径，避免 next=/ui/login 自指形成重定向环
    const next = typeof to.query.next === 'string' ? to.query.next : to.fullPath
    const safe = next.startsWith('/') && !next.startsWith('/login') ? next : '/'
    return { path: '/login', query: { next: safe } }
  }
  return true
})

export default router

import { createPinia } from 'pinia'
import { createApp } from 'vue'

import App from './App.vue'
import router from './router'
import './styles/main.css'

// ElMessage / ElMessageBox 是函数式调用（不经过模板），unplugin 的
// ElementPlusResolver 只给模板中的组件标签注入样式，这两个组件的
// overlay/卡片/定位 CSS 永远不会被引入——弹窗会退化成贴左缘无样式的
// 裸内容（见 gui-smoke 截图证据）。显式引入完整样式。
import 'element-plus/es/components/message/style/css'
import 'element-plus/es/components/message-box/style/css'
import 'element-plus/es/components/notification/style/css'

const app = createApp(App)
app.use(createPinia())
app.use(router)
app.mount('#app')

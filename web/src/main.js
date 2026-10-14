import { createApp } from 'vue'
import { createPinia } from 'pinia'
import TDesign from 'tdesign-vue-next'
import 'tdesign-vue-next/es/style/index.css'

import App from './App.vue'
import AppIcon from './components/common/AppIcon'
import router from './router'
import './styles/global.css'
import { applySavedBrand } from './utils/brand'

applySavedBrand()

const app = createApp(App)
app.use(createPinia())
app.use(router)
app.use(TDesign)
// The kebab-case alias resolves before TDesign's CDN-backed TIcon registration.
app.component('t-icon', AppIcon)
app.mount('#app')

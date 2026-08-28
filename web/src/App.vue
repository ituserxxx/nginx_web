<script setup>
import { ref, computed, onMounted } from 'vue'

const message = ref('加载中...')
const nginx = ref(null)
const available = ref(null)
const selectedKeys = ref(['detect'])

const detectColumns = [
  { title: '#', key: 'index', customRender: ({ index }) => index + 1, width: 60 },
  { title: '版本', dataIndex: 'version' },
  { title: '执行目录', dataIndex: 'path' },
]

const availableColumns = [{ title: '版本号', dataIndex: 'version' }]

const availableRows = computed(() =>
  (available.value?.versions ?? []).map((v) => ({ version: v }))
)

onMounted(async () => {
  try {
    const res = await fetch('/api/hello')
    const data = await res.json()
    message.value = data.message
  } catch {
    message.value = '后端连接失败'
  }

  try {
    const res = await fetch('/api/nginx')
    nginx.value = await res.json()
  } catch {
    nginx.value = { supported: false, installed: false, instances: [] }
  }

  try {
    const res = await fetch('/api/nginx/available')
    available.value = await res.json()
  } catch {
    available.value = { supported: false, manager: '', versions: [] }
  }
})
</script>

<template>
  <a-layout style="min-height: 100vh">
    <a-layout-sider theme="dark">
      <div class="logo">nginx_web</div>
      <a-menu v-model:selectedKeys="selectedKeys" theme="dark" mode="inline">
        <a-menu-item key="detect">Nginx 检测</a-menu-item>
        <a-menu-item key="available">可安装版本</a-menu-item>
      </a-menu>
    </a-layout-sider>

    <a-layout>
      <a-layout-header class="header">{{ message }}</a-layout-header>

      <a-layout-content class="content">
        <template v-if="selectedKeys[0] === 'detect'">
          <h2>Nginx 检测结果</h2>
          <a-spin v-if="nginx === null" tip="检测中..." />
          <a-alert
            v-else-if="!nginx.supported"
            type="warning"
            show-icon
            message="该功能仅支持 Linux（含 Ubuntu）系统"
          />
          <a-alert v-else-if="!nginx.installed" type="info" show-icon message="未检测到 Nginx 安装" />
          <a-table
            v-else
            :columns="detectColumns"
            :data-source="nginx.instances"
            row-key="path"
            :pagination="false"
          />
        </template>

        <template v-else>
          <h2>可安装的 Nginx 版本</h2>
          <a-spin v-if="available === null" tip="查询中..." />
          <a-alert
            v-else-if="!available.supported"
            type="warning"
            show-icon
            message="该功能仅支持 Linux（含 Ubuntu）系统"
          />
          <a-alert
            v-else-if="!available.manager"
            type="info"
            show-icon
            message="当前系统未检测到支持的包管理器（支持 apt / dnf / yum / apk）"
          />
          <template v-else>
            <p>包管理器：{{ available.manager }}</p>
            <a-alert v-if="availableRows.length === 0" type="info" show-icon message="未查询到可安装版本" />
            <a-table
              v-else
              :columns="availableColumns"
              :data-source="availableRows"
              row-key="version"
              :pagination="false"
            />
          </template>
        </template>
      </a-layout-content>
    </a-layout>
  </a-layout>
</template>

<style scoped>
.logo {
  color: #fff;
  font-size: 18px;
  font-weight: 600;
  text-align: center;
  padding: 16px 0;
}

.header {
  background: #fff;
  display: flex;
  align-items: center;
  font-size: 16px;
  padding: 0 24px;
}

.content {
  margin: 24px;
  padding: 24px;
  background: #fff;
  border-radius: 8px;
}
</style>

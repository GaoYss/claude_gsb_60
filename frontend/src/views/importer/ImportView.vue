<template>
  <div class="page">
    <PageHeader title="台账迁移" description="将旧系统的故障与维修台账按模板一次性批量导入: 逐行校验、整批原子写入、按条数与金额核对">
      <el-button :icon="Download" @click="importerApi.downloadTemplate">下载模板</el-button>
      <el-button :icon="Refresh" @click="load">刷新</el-button>
    </PageHeader>

    <el-card shadow="never">
      <div class="upload-area">
        <el-upload
          ref="uploadRef"
          drag
          accept=".csv"
          :auto-upload="false"
          :show-file-list="false"
          :on-change="handleFileChange"
        >
          <el-icon :size="40" class="upload-icon"><UploadFilled /></el-icon>
          <div class="el-upload__text">将台账 CSV 文件拖到此处, 或 <em>点击选择文件</em></div>
        </el-upload>
        <div class="upload-side">
          <div v-if="selectedName" class="selected-file">
            <el-icon><Document /></el-icon>
            <span>{{ selectedName }}</span>
          </div>
          <div class="upload-actions">
            <el-button type="primary" :icon="Upload" :disabled="!selectedFile" :loading="importing" @click="startImport">
              开始导入
            </el-button>
            <el-button :disabled="!selectedFile || importing" @click="clearFile">重新选择</el-button>
          </div>
          <div class="upload-tips text-muted">
            请使用模板填写, 支持 UTF-8 / GBK 编码的 CSV 文件, 单批最多 5000 行。<br />
            任何一行校验不通过整批都不会写入; 同一份文件重复上传会自动识别并跳过。
          </div>
        </div>
      </div>
    </el-card>

    <el-card v-if="result" shadow="never">
      <el-alert type="success" :closable="false" title="导入完成, 整批已写入" />
      <el-descriptions :column="3" border class="result-descriptions">
        <el-descriptions-item label="批次号">{{ result.batch_no }}</el-descriptions-item>
        <el-descriptions-item label="数据行数">{{ result.total_rows }}</el-descriptions-item>
        <el-descriptions-item label="导入故障">{{ result.fault_count }} 条</el-descriptions-item>
        <el-descriptions-item label="其中未闭环">{{ result.open_fault_count }} 条</el-descriptions-item>
        <el-descriptions-item label="导入维修记录">{{ result.repair_count }} 条</el-descriptions-item>
        <el-descriptions-item label="维修费用合计">{{ formatMoney(result.total_cost) }}</el-descriptions-item>
      </el-descriptions>
      <div class="result-hint text-muted">
        请与旧台账按条数与金额合计核对, 确认没有漏项。导入的记录已计入亮灯率基数, 不参与逾期统计。
      </div>
    </el-card>

    <el-card v-if="duplicated" shadow="never">
      <el-alert type="warning" :closable="false" title="该文件之前已导入过, 本次已自动跳过, 未重复写入" />
      <el-descriptions :column="3" border class="result-descriptions">
        <el-descriptions-item label="批次号">{{ duplicated.batch_no }}</el-descriptions-item>
        <el-descriptions-item label="导入故障">{{ duplicated.fault_count }} 条</el-descriptions-item>
        <el-descriptions-item label="维修费用合计">{{ formatMoney(duplicated.total_cost) }}</el-descriptions-item>
        <el-descriptions-item label="导入时间">{{ formatDateTime(duplicated.created_at) }}</el-descriptions-item>
        <el-descriptions-item label="文件名" :span="2">{{ duplicated.file_name }}</el-descriptions-item>
      </el-descriptions>
    </el-card>

    <el-card v-if="failure" shadow="never">
      <el-alert
        type="error"
        :closable="false"
        :title="`校验未通过: 共 ${failure.total_rows} 行数据, 其中 ${failure.error_rows} 行存在问题, 整批未写入`"
      />
      <el-table :data="failure.row_errors" stripe class="error-table">
        <el-table-column prop="line_no" label="行号" width="90" align="center" />
        <el-table-column prop="lamp_code" label="路灯编号" width="130" />
        <el-table-column label="问题原因">
          <template #default="{ row }">
            <div v-for="(reason, index) in row.reasons" :key="index" class="error-reason">{{ reason }}</div>
          </template>
        </el-table-column>
      </el-table>
      <div class="result-hint text-muted">请按上表修改文件后重新上传, 校验全部通过前系统不会写入任何数据。</div>
    </el-card>

    <el-card shadow="never">
      <div class="section-title">导入记录</div>
      <el-table v-loading="loading" :data="rows" stripe>
        <el-table-column prop="batch_no" label="批次号" width="160" />
        <el-table-column prop="file_name" label="文件名" min-width="180" show-overflow-tooltip />
        <el-table-column prop="total_rows" label="数据行数" width="90" align="center" />
        <el-table-column prop="fault_count" label="故障条数" width="90" align="center" />
        <el-table-column prop="open_fault_count" label="未闭环" width="80" align="center" />
        <el-table-column prop="repair_count" label="维修条数" width="90" align="center" />
        <el-table-column label="费用合计" width="120" align="right">
          <template #default="{ row }">{{ formatMoney(row.total_cost) }}</template>
        </el-table-column>
        <el-table-column label="导入时间" width="150">
          <template #default="{ row }">{{ formatDateTime(row.created_at) }}</template>
        </el-table-column>
      </el-table>
      <DataPagination
        :page="query.page"
        :page-size="query.page_size"
        :total="total"
        @page-change="changePage"
        @size-change="changePageSize"
      />
    </el-card>
  </div>
</template>

<script setup>
import { ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Document, Download, Refresh, Upload, UploadFilled } from '@element-plus/icons-vue'
import PageHeader from '@/components/common/PageHeader.vue'
import DataPagination from '@/components/common/DataPagination.vue'
import { importerApi } from '@/api/importer'
import { formatDateTime, formatMoney } from '@/utils/format'
import { useListPage } from '@/composables/useListPage'

const { loading, rows, total, query, load, changePage, changePageSize } = useListPage(importerApi.batches, {})

const uploadRef = ref(null)
const selectedFile = ref(null)
const selectedName = ref('')
const importing = ref(false)
const result = ref(null)
const duplicated = ref(null)
const failure = ref(null)

function handleFileChange(uploadFile) {
  selectedFile.value = uploadFile.raw
  selectedName.value = uploadFile.name
  clearOutcome()
}

function clearFile() {
  selectedFile.value = null
  selectedName.value = ''
  uploadRef.value?.clearFiles()
}

function clearOutcome() {
  result.value = null
  duplicated.value = null
  failure.value = null
}

async function startImport() {
  if (!selectedFile.value || importing.value) return
  importing.value = true
  clearOutcome()
  try {
    const data = await importerApi.upload(selectedFile.value)
    if (data.duplicated) {
      duplicated.value = data.batch
      ElMessage.warning('该文件之前已导入过, 本次已跳过')
    } else {
      result.value = data.batch
      ElMessage.success(`导入完成: ${data.batch.fault_count} 条故障 / ${data.batch.repair_count} 条维修`)
    }
    clearFile()
    load()
  } catch (error) {
    if (error.data?.row_errors) {
      failure.value = error.data
    }
    ElMessage.error(error.message)
  } finally {
    importing.value = false
  }
}
</script>

<style scoped>
.upload-area {
  display: flex;
  gap: 24px;
  align-items: stretch;
}

.upload-area :deep(.el-upload) {
  width: 320px;
}

.upload-area :deep(.el-upload-dragger) {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  height: 160px;
}

.upload-icon {
  color: var(--el-color-primary);
  margin-bottom: 8px;
}

.upload-side {
  flex: 1;
  display: flex;
  flex-direction: column;
  justify-content: center;
  gap: 12px;
}

.selected-file {
  display: flex;
  align-items: center;
  gap: 6px;
  font-weight: 600;
}

.upload-actions {
  display: flex;
  gap: 8px;
}

.upload-tips {
  font-size: 12px;
  line-height: 1.8;
}

.result-descriptions {
  margin-top: 12px;
}

.result-hint {
  margin-top: 12px;
  font-size: 13px;
}

.error-table {
  margin-top: 12px;
}

.error-reason {
  color: var(--el-color-danger);
  line-height: 1.7;
}

.section-title {
  font-weight: 600;
  margin-bottom: 12px;
}
</style>

<template>
  <div class="page">
    <PageHeader title="历史台账导入" description="将旧系统的故障与维修台账按模板一次性迁入, 逐行校验、整批生效、按条数与金额核对">
      <el-button :icon="Download" tag="a" :href="templateUrl" download>下载导入模板</el-button>
    </PageHeader>

    <el-card shadow="never" class="step-card">
      <el-steps :active="activeStep" align-center finish-status="success">
        <el-step title="下载模板" description="按列填写旧台账" />
        <el-step title="上传预检" description="逐行校验并核对条数/金额" />
        <el-step title="确认导入" description="整批生效, 重复文件自动跳过" />
      </el-steps>
    </el-card>

    <el-card shadow="never">
      <div class="upload-bar">
        <el-upload
          ref="uploadRef"
          :auto-upload="false"
          :limit="1"
          accept=".xlsx"
          :on-change="handleFileChange"
          :on-exceed="handleExceed"
          :on-remove="handleRemove"
          :file-list="fileList"
        >
          <el-button type="primary" :icon="Upload2">选择台账文件(.xlsx)</el-button>
          <template #tip>
            <div class="upload-tip">选择后自动逐行校验, 不会写入数据; 单次最多 5000 行、20MB</div>
          </template>
        </el-upload>
        <el-input
          v-model="operator"
          placeholder="操作人(可选)"
          class="operator-input"
          clearable
        />
        <el-button
          type="success"
          :icon="Check"
          :disabled="!preview || (!preview.ready && !preview.duplicate_of)"
          :loading="committing"
          @click="handleCommit"
        >
          {{ preview?.duplicate_of ? '确认重复并跳过' : '确认导入' }}
        </el-button>
        <el-button :icon="RefreshLeft" @click="resetAll">重置</el-button>
      </div>

      <el-alert
        v-if="preview?.duplicate_of"
        type="warning"
        :closable="false"
        show-icon
        class="result-alert"
        :title="`该文件内容已在批次 ${preview.duplicate_of} 导入过, 再次导入会自动跳过, 不会产生重复记录`"
      />

      <template v-if="preview">
        <el-row :gutter="12" class="summary-row">
          <el-col :xs="12" :sm="8" :md="4">
            <div class="summary-box">
              <div class="summary-box__value">{{ preview.total_rows }}</div>
              <div class="summary-box__label">数据行数</div>
            </div>
          </el-col>
          <el-col :xs="12" :sm="8" :md="4">
            <div class="summary-box">
              <div class="summary-box__value" :class="{ 'is-error': !preview.ready }">
                {{ preview.issues.length }}
              </div>
              <div class="summary-box__label">问题行</div>
            </div>
          </el-col>
          <el-col :xs="12" :sm="8" :md="5">
            <div class="summary-box">
              <div class="summary-box__value">{{ preview.summary.fault_count }}</div>
              <div class="summary-box__label">将导入故障(条)</div>
            </div>
          </el-col>
          <el-col :xs="12" :sm="8" :md="5">
            <div class="summary-box">
              <div class="summary-box__value">{{ preview.summary.repair_count }}</div>
              <div class="summary-box__label">将导入维修(条)</div>
            </div>
          </el-col>
          <el-col :xs="12" :sm="8" :md="6">
            <div class="summary-box">
              <div class="summary-box__value is-money">{{ formatMoney(preview.summary.total_cost) }}</div>
              <div class="summary-box__label">维修费用合计(核对基数)</div>
            </div>
          </el-col>
        </el-row>
        <div v-if="preview.example_skipped" class="example-tip">
          已自动忽略模板示例行 {{ preview.example_skipped }} 行
        </div>

        <el-alert
          v-if="preview.ready && !preview.duplicate_of"
          type="success"
          :closable="false"
          show-icon
          title="校验通过: 点击「确认导入」后整批写入; 导入结束会再按条数与金额合计回查核对, 不一致将整体回滚"
          class="result-alert"
        />

        <template v-if="preview.issues.length">
          <div class="issue-title">
            <el-tag type="danger" effect="dark">{{ preview.issues.length }} 行存在问题</el-tag>
            <span class="text-muted">请按下表原因修改原文件后, 重新选择文件上传(无需删除正确的行)</span>
          </div>
          <el-table :data="preview.issues" stripe max-height="360" class="issue-table">
            <el-table-column prop="row" label="Excel 行号" width="110" />
            <el-table-column prop="lamp_code" label="路灯编号" width="140" />
            <el-table-column label="问题原因(逐行可读)">
              <template #default="{ row }">
                <div v-for="(message, index) in row.messages" :key="index" class="issue-message">
                  <el-icon class="issue-icon"><CircleCloseFilled /></el-icon>
                  <span>{{ message }}</span>
                </div>
              </template>
            </el-table-column>
          </el-table>
        </template>
      </template>

      <el-alert
        v-if="commitResult"
        :type="commitResult.skipped ? 'warning' : 'success'"
        :closable="false"
        show-icon
        class="result-alert"
        :title="commitResult.message"
      >
        <div class="commit-detail">
          <span>故障 {{ commitResult.summary.fault_count }} 条</span>
          <span>维修 {{ commitResult.summary.repair_count }} 条</span>
          <span>费用合计 {{ formatMoney(commitResult.summary.total_cost) }}</span>
          <span v-if="commitResult.batch">批次号 {{ commitResult.batch.batch_no }}</span>
        </div>
      </el-alert>
    </el-card>

    <ImportBatchTable ref="batchTable" />
  </div>
</template>

<script setup>
import { computed, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Check, Download, RefreshLeft, Upload as Upload2, CircleCloseFilled } from '@element-plus/icons-vue'
import PageHeader from '@/components/common/PageHeader.vue'
import ImportBatchTable from './components/ImportBatchTable.vue'
import { legacyApi } from '@/api/legacy'
import { formatMoney } from '@/utils/format'

const templateUrl = legacyApi.templateUrl()

const uploadRef = ref()
const batchTable = ref()
const fileList = ref([])
const selectedFile = ref(null)
const operator = ref('')
const preview = ref(null)
const commitResult = ref(null)
const committing = ref(false)

const activeStep = computed(() => {
  if (commitResult.value && !commitResult.value.skipped) return 3
  if (preview.value) return 2
  return 1
})

async function runPreview(file) {
  try {
    commitResult.value = null
    preview.value = await legacyApi.preview(file)
    if (preview.value.ready && !preview.value.duplicate_of) {
      ElMessage.success(`校验通过: 故障 ${preview.value.summary.fault_count} 条 / 维修 ${preview.value.summary.repair_count} 条`)
    } else if (preview.value.duplicate_of) {
      ElMessage.warning('该文件已导入过, 确认导入将自动跳过')
    } else {
      ElMessage.error(`发现 ${preview.value.issues.length} 行问题, 请修改后重新上传`)
    }
  } catch (error) {
    preview.value = null
  }
}

function handleFileChange(uploadFile) {
  if (!uploadFile.raw) return
  selectedFile.value = uploadFile.raw
  fileList.value = [uploadFile]
  runPreview(uploadFile.raw)
}

function handleExceed(files) {
  uploadRef.value?.clearFiles()
  const file = files[0]
  fileList.value = [{ name: file.name, raw: file }]
  selectedFile.value = file
  runPreview(file)
}

function handleRemove() {
  resetAll()
}

async function handleCommit() {
  if (!selectedFile.value || !preview.value) return
  committing.value = true
  try {
    commitResult.value = await legacyApi.commit(selectedFile.value, operator.value)
    if (!commitResult.value.skipped) {
      ElMessage.success('历史台账导入完成')
      uploadRef.value?.clearFiles()
      fileList.value = []
      selectedFile.value = null
      preview.value = null
      batchTable.value?.load()
    } else {
      ElMessage.warning('重复文件, 已自动跳过')
      preview.value = null
    }
  } finally {
    committing.value = false
  }
}

function resetAll() {
  uploadRef.value?.clearFiles()
  fileList.value = []
  selectedFile.value = null
  preview.value = null
  commitResult.value = null
}
</script>

<style scoped>
.step-card {
  margin: 16px 0;
}

.upload-bar {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}

.operator-input {
  width: 180px;
}

.upload-tip {
  margin-top: 6px;
  font-size: 12px;
  color: var(--app-text-secondary, #909399);
}

.summary-row {
  margin-top: 16px;
}

.summary-box {
  background: var(--app-fill, #f5f7fa);
  border-radius: 6px;
  padding: 14px 12px;
  text-align: center;
}

.summary-box__value {
  font-size: 22px;
  font-weight: 700;
  color: #303133;
}

.summary-box__value.is-error {
  color: #f56c6c;
}

.summary-box__value.is-money {
  font-size: 18px;
  color: #409eff;
}

.summary-box__label {
  margin-top: 4px;
  font-size: 12px;
  color: #909399;
}

.example-tip {
  margin-top: 10px;
  font-size: 12px;
  color: #909399;
}

.result-alert {
  margin-top: 16px;
}

.commit-detail {
  display: flex;
  gap: 18px;
  flex-wrap: wrap;
  margin-top: 4px;
  font-size: 13px;
}

.issue-title {
  display: flex;
  align-items: center;
  gap: 10px;
  margin: 18px 0 10px;
  font-size: 14px;
}

.issue-table {
  border: 1px solid var(--app-border, #ebeef5);
  border-radius: 4px;
}

.issue-message {
  display: flex;
  align-items: flex-start;
  gap: 6px;
  line-height: 20px;
  padding: 2px 0;
}

.issue-icon {
  margin-top: 3px;
  color: #f56c6c;
  flex-shrink: 0;
}
</style>

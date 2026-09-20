<template>
  <el-card shadow="never" class="batch-card">
    <div class="batch-header">
      <span class="batch-title">导入批次记录</span>
      <div class="batch-filter">
        <el-input
          v-model="query.keyword"
          placeholder="批次号 / 文件名 / 操作人"
          clearable
          class="batch-search"
          @keyup.enter="search"
        />
        <el-button type="primary" :icon="Search" @click="search">查询</el-button>
        <el-button :icon="Refresh" @click="reset">重置</el-button>
      </div>
    </div>

    <el-table v-loading="loading" :data="rows" stripe>
      <el-table-column prop="batch_no" label="批次号" width="170" fixed="left" />
      <el-table-column prop="file_name" label="文件名" min-width="200" show-overflow-tooltip />
      <el-table-column prop="total_rows" label="数据行" width="90" align="center" />
      <el-table-column prop="fault_count" label="故障条数" width="90" align="center" />
      <el-table-column prop="repair_count" label="维修条数" width="90" align="center" />
      <el-table-column label="费用合计" width="120" align="right">
        <template #default="{ row }">{{ formatMoney(row.total_cost) }}</template>
      </el-table-column>
      <el-table-column prop="operator" label="操作人" width="100" />
      <el-table-column label="导入时间" width="160">
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
</template>

<script setup>
import { Refresh, Search } from '@element-plus/icons-vue'
import DataPagination from '@/components/common/DataPagination.vue'
import { legacyApi } from '@/api/legacy'
import { formatDateTime, formatMoney } from '@/utils/format'
import { useListPage } from '@/composables/useListPage'

const { loading, rows, total, query, load, search, reset, changePage, changePageSize } = useListPage(
  legacyApi.listBatches,
  { keyword: '' },
)

defineExpose({ load })
</script>

<style scoped>
.batch-card {
  margin-top: 16px;
}

.batch-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 14px;
  flex-wrap: wrap;
}

.batch-title {
  font-size: 15px;
  font-weight: 600;
}

.batch-filter {
  display: flex;
  gap: 8px;
}

.batch-search {
  width: 240px;
}
</style>

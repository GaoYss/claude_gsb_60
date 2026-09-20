import request from './request'

// 历史故障/维修台账批量导入接口。
export const legacyApi = {
  // 下载导入模板, 返回二进制 Blob。
  templateUrl: () => `${request.defaults.baseURL}/legacy/template`,

  // 逐行校验文件但不写库。
  preview: (file) => {
    const form = new FormData()
    form.append('file', file)
    return request.post('/legacy/imports/preview', form, {
      headers: { 'Content-Type': 'multipart/form-data' },
      timeout: 60000,
    })
  },

  // 确认导入: 整批事务写入, 重复文件自动跳过。
  commit: (file, operator = '') => {
    const form = new FormData()
    form.append('file', file)
    if (operator) form.append('operator', operator)
    return request.post('/legacy/imports', form, {
      headers: { 'Content-Type': 'multipart/form-data' },
      timeout: 120000,
    })
  },

  // 导入批次记录。
  listBatches: (params) => request.get('/legacy/imports', { params }),
  batchDetail: (id) => request.get(`/legacy/imports/${id}`),
}

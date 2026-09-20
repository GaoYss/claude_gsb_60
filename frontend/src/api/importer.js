import request from './request'

const baseURL = import.meta.env.VITE_API_BASE_URL || '/api/v1'

// 台账迁移导入接口。
export const importerApi = {
  // 下载导入模板(浏览器直接打开附件地址)。
  downloadTemplate: () => window.open(`${baseURL}/imports/template`, '_blank'),
  // 上传台账文件执行批量导入, 大文件放宽超时。
  upload: (file) => {
    const form = new FormData()
    form.append('file', file)
    return request.post('/imports/legacy', form, {
      headers: { 'Content-Type': 'multipart/form-data' },
      timeout: 120000,
      silent: true,
    })
  },
  batches: (params) => request.get('/imports/batches', { params }),
  batch: (id) => request.get(`/imports/batches/${id}`),
}

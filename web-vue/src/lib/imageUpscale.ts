export type ImageUpscaleEngine = 'go_lanczos3' | 'sharp_lanczos3' | 'pillow_lanczos' | 'fsrcnn_x2'

// Match the Go normalizer, including short names saved by earlier dashboards.
export function normalizeImageUpscaleEngine(value: unknown): ImageUpscaleEngine {
  switch (String(value ?? '').trim().toLowerCase()) {
    case 'sharp':
    case 'sharp_lanczos3': return 'sharp_lanczos3'
    case 'pillow':
    case 'pillow_lanczos': return 'pillow_lanczos'
    case 'fsrcnn':
    case 'fsrcnn_x2': return 'fsrcnn_x2'
    default: return 'go_lanczos3'
  }
}

export const imageUpscaleEngineOptions = [
  { value: 'go_lanczos3', label: 'Go · Lanczos3（内置）' },
  { value: 'sharp_lanczos3', label: 'Sharp · Lanczos3' },
  { value: 'pillow_lanczos', label: 'Pillow · Lanczos' },
  { value: 'fsrcnn_x2', label: 'FSRCNN · x2' },
]

export const imageUpscaleEngineHints: Record<ImageUpscaleEngine, string> = {
  go_lanczos3: '内置 Lanczos3 插值放大，无需额外图片处理服务。',
  sharp_lanczos3: '使用图片处理服务的 Sharp Lanczos3；请确保 image-processor 已启动。',
  pillow_lanczos: '使用图片处理服务的 Pillow Lanczos；请确保 image-processor 已启动。',
  fsrcnn_x2: '使用 FSRCNN x2 模型放大后调整至请求尺寸，需要图片处理服务，耗时通常高于插值。',
}

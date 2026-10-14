import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import vm from 'node:vm'
import ts from 'typescript'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
function load(relative, dependencies = {}) {
  const source = fs.readFileSync(path.join(root, relative), 'utf8')
  const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } })
  const module = { exports: {} }
  const require = (name) => {
    if (!Object.hasOwn(dependencies, name)) throw new Error(`Unexpected dependency ${name}`)
    return dependencies[name]
  }
  vm.runInNewContext(outputText, { exports: module.exports, module, require }, { filename: relative })
  return module.exports
}
const engines = load('src/lib/imageUpscale.ts')
const settings = load('src/api/settings.ts', { './client': {}, '@/lib/imageUpscale': engines })
const cases = [
  ['', 'go_lanczos3'], ['go', 'go_lanczos3'], ['go_lanczos3', 'go_lanczos3'],
  ['sharp', 'sharp_lanczos3'], ['sharp_lanczos3', 'sharp_lanczos3'],
  ['pillow', 'pillow_lanczos'], ['pillow_lanczos', 'pillow_lanczos'],
  ['fsrcnn', 'fsrcnn_x2'], ['fsrcnn_x2', 'fsrcnn_x2'], ['  SHARP  ', 'sharp_lanczos3'],
]
for (const [input, expected] of cases) {
  assert.equal(engines.normalizeImageUpscaleEngine(input), expected)
  const loaded = settings.normalizeSettings({ image_upscale_engine: input, image_upscale_enabled: true, image_account_concurrency: 7 })
  assert.equal(loaded.image_upscale_engine, expected)
  const saved = settings.prepareSettingsForSave(loaded)
  assert.equal(saved.image_upscale_engine, expected)
  assert.equal(saved.image_account_concurrency, 7)
  assert.equal(settings.normalizeSettings(saved).image_upscale_engine, expected)
  const patch = settings.prepareSettingsPatch({ ...loaded, image_retention_days: loaded.image_retention_days + 1 }, loaded)
  assert.equal(Object.hasOwn(patch, 'image_upscale_engine'), false, 'Unrelated save resets engine')
}
assert.equal(settings.normalizeSettings({}).image_upscale_engine, 'go_lanczos3')
assert.equal(new Set(engines.imageUpscaleEngineOptions.map(option => option.value)).size, 4)
for (const option of engines.imageUpscaleEngineOptions) {
  assert.equal(engines.normalizeImageUpscaleEngine(option.value), option.value)
  assert.ok(engines.imageUpscaleEngineHints[option.value])
}
console.log(`PASS: ${cases.length} engine mappings, settings read/save round trips, unrelated setting preservation, and all selector options`)

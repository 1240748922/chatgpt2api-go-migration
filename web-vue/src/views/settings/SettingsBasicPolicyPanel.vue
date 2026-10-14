<template>
  <div class="space-y-4">
    <FormSection title="账号策略">
      <div class="settings-check-grid settings-check-grid--single">
        <div class="settings-check-item">
          <div class="settings-check-control">
            <Checkbox v-model="settings.auto_remove_invalid_accounts">自动移除异常账号</Checkbox>
            <HelpTip text="确认鉴权无效的账号会进入异常处理；开启后直接移除，关闭后保留异常状态。" />
          </div>
        </div>
        <div class="settings-check-item">
          <div class="settings-check-control">
            <Checkbox v-model="settings.auto_remove_rate_limited_accounts">自动移除额度耗尽账号</Checkbox>
            <HelpTip text="只有远程明确确认图片额度为 0 时才会处理，代理错误、断流或上游 429 不会删除账号。" />
          </div>
        </div>
      </div>
    </FormSection>

    <FormSection title="图片超分">
      <div class="settings-check-grid settings-check-grid--single">
        <div class="settings-check-item">
          <div class="settings-check-control">
            <Checkbox v-model="settings.image_upscale_enabled">按请求尺寸放大图片</Checkbox>
            <HelpTip text="仅在图片小于请求尺寸时放大；关闭后返回上游原图。插值放大不代表重新生成更多细节。" />
          </div>
        </div>
      </div>
      <FormField label="处理引擎">
        <div class="w-full">
          <GroupedSelectMenu
            v-model="selectedEngine"
            :options="imageUpscaleEngineOptions"
            :disabled="!settings.image_upscale_enabled"
            aria-label="图片超分处理引擎"
            block
          />
        </div>
      </FormField>
      <p class="text-xs leading-relaxed text-muted-foreground">{{ imageUpscaleEngineHints[selectedEngine] }}</p>
    </FormSection>

    <FormSection title="图片确认">
      <div class="settings-check-grid settings-check-grid--single">
        <div class="settings-check-item">
          <div class="settings-check-control">
            <Checkbox v-model="settings.image_settle_enabled">图片二次确认机制</Checkbox>
            <HelpTip text="找到图片结果后再等待指定秒数复查一次，减少结果尚未稳定时提前返回。" />
          </div>
        </div>
        <div class="settings-check-item">
          <div class="settings-check-control">
            <Checkbox v-model="settings.image_remove_conversation_after_result">图片成功后删除官网会话</Checkbox>
            <HelpTip text="默认关闭。仅在图片已成功保存后尝试隐藏 ChatGPT 官网 conversation；失败只记录日志，不影响图片返回。关闭时保留官网会话，便于恢复和排查。" />
          </div>
        </div>
      </div>
      <FormField label="二次确认等待（秒）">
        <Input
          :model-value="imageSettleSecondsField.input.value"
          type="number"
          block
          placeholder="2"
          :disabled="!settings.image_settle_enabled"
          @update:model-value="imageSettleSecondsField.update"
        />
      </FormField>
    </FormSection>

    <FormSection title="控制台日志级别">
      <div class="settings-check-grid settings-check-grid--single mt-3">
        <div
          v-for="level in logLevelOptions"
          :key="level"
          class="settings-check-item"
        >
          <div class="settings-check-control">
            <Checkbox
              :model-value="settings.log_levels.includes(level)"
              @update:model-value="$emit('setLogLevel', level, Boolean($event))"
            >
              {{ level }}
            </Checkbox>
            <HelpTip v-if="level === 'debug'" text="不选择任何级别时使用默认 info / warning / error。" />
          </div>
        </div>
      </div>
    </FormSection>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { Checkbox, FormField, FormSection, HelpTip, Input } from 'nanocat-ui'
import GroupedSelectMenu from '@/components/ui/GroupedSelectMenu.vue'
import { imageUpscaleEngineHints, imageUpscaleEngineOptions, normalizeImageUpscaleEngine } from '@/lib/imageUpscale'
import type { Settings } from '@/types/api'
import { logLevelOptions } from '@/views/settings/settingsView'
import type { NumberSettingField } from '@/views/settings/useNumberSettingField'

const props = defineProps<{
  settings: Settings
  imageSettleSecondsField: NumberSettingField
}>()

const selectedEngine = computed({
  get: () => normalizeImageUpscaleEngine(props.settings.image_upscale_engine),
  set: (value: string | string[]) => {
    props.settings.image_upscale_engine = normalizeImageUpscaleEngine(value)
  },
})

defineEmits<{
  setLogLevel: [level: string, enabled: boolean]
}>()
</script>

<style scoped>
.settings-check-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(13.5rem, 1fr));
  gap: 8px;
}

.settings-check-grid--single {
  grid-template-columns: minmax(0, 1fr);
}

.settings-check-item {
  min-height: 38px;
  border: 1px solid hsl(var(--border));
  border-radius: 14px;
  background: hsl(var(--background) / 0.72);
  transition:
    border-color 0.16s ease,
    background-color 0.16s ease;
}

.settings-check-item:hover {
  border-color: hsl(var(--foreground) / 0.18);
  background: hsl(var(--muted) / 0.24);
}

.settings-check-control {
  display: flex;
  min-height: 38px;
  align-items: center;
  gap: 8px;
  padding-right: 10px;
}

.settings-check-item :deep(label) {
  display: flex;
  width: 100%;
  flex: 1;
  min-height: 38px;
  align-items: center;
  gap: 10px;
  padding: 9px 11px;
}

.settings-check-item :deep(label > span:last-child) {
  color: hsl(var(--foreground) / 0.78);
  line-height: 1.35;
}
</style>

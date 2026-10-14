/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { CircleAlert, CircleCheck, RefreshCw } from 'lucide-react'
import { useEffect, useState } from 'react'
import type { UseFormReturn } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import {
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
} from '@/components/ui/form'
import { Label } from '@/components/ui/label'
import { RadioGroup, RadioGroupItem } from '@/components/ui/radio-group'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { cn } from '@/lib/utils'

import { testChannel } from '../../../api'
import {
  getTworkOMPFormWires,
  parseModelsList,
  TWORK_OMP_WIRE_LABELS,
  type ChannelFormValues,
  type TworkOMPWire,
} from '../../../lib'

type DetectResult =
  | { kind: 'responses-ok'; model: string; time?: number }
  | { kind: 'suggest-chat'; model: string; error?: string }
  | { kind: 'failed'; model: string; error?: string }
  | { kind: 'no-model' }

const WIRE_HINTS: Record<TworkOMPWire, string> = {
  responses: 'Preferred when the upstream supports it',
  'chat-completions':
    'Widest compatibility; use when the upstream does not support Responses',
  'anthropic-messages': 'Fixed for this channel type',
}

// 默认的按下态只有浅灰底，选中项在抽屉里不够醒目。
const RUNTIME_TOGGLE_ITEM =
  'text-muted-foreground px-4 aria-pressed:border-foreground aria-pressed:bg-background aria-pressed:text-foreground data-[pressed]:border-foreground data-[pressed]:bg-background data-[pressed]:text-foreground'

type TworkRuntimeFieldProps = {
  form: UseFormReturn<ChannelFormValues>
  /** Saved channel id; detection runs only on saved channels. */
  channelId?: number
}

/**
 * Twork runtime of the channel. OMP channels serve only new Twork clients;
 * Pi / Codex / async image channels stay managed by Twork CMS.
 */
export function TworkRuntimeField(props: TworkRuntimeFieldProps) {
  const { t } = useTranslation()
  const { form, channelId } = props
  const channelType = form.watch('type')
  const runtime = form.watch('twork_runtime')
  const initialRuntime = form.watch('twork_runtime_initial')
  const managedRuntime = form.watch('twork_managed_runtime')
  const wire = form.watch('twork_wire_api') as TworkOMPWire | ''
  const wires = getTworkOMPFormWires(channelType)
  const [detecting, setDetecting] = useState(false)
  const [detectResult, setDetectResult] = useState<DetectResult | null>(null)

  // 类型变化后协议必须仍属于该类型；不支持 OMP 的类型回到旧版。
  useEffect(() => {
    if (runtime !== 'omp') return
    if (wires.length === 0) {
      form.setValue('twork_runtime', 'legacy', { shouldDirty: true })
      return
    }
    if (!wires.includes(wire as TworkOMPWire)) {
      form.setValue('twork_wire_api', wires[0], { shouldDirty: true })
    }
  }, [channelType, runtime, wire, wires, form])

  if (runtime === 'managed') {
    return (
      <div className='flex flex-col gap-1.5'>
        <Label>{t('Twork Runtime')}</Label>
        <div className='text-muted-foreground rounded-md border border-dashed px-3 py-2 text-sm'>
          {t('{{runtime}} (managed in Twork CMS)', {
            runtime: managedRuntime,
          })}
        </div>
      </div>
    )
  }

  if (wires.length === 0) {
    return (
      <div className='flex flex-col gap-1.5'>
        <Label>{t('Twork Runtime')}</Label>
        <p className='text-muted-foreground text-xs'>
          {t(
            'This channel type does not support New OMP. Use the OpenAI-compatible type if needed.'
          )}
        </p>
      </div>
    )
  }

  const detect = async () => {
    if (!channelId) return
    const model =
      form.getValues('test_model')?.trim() ||
      parseModelsList(form.getValues('models'))[0]
    if (!model) {
      setDetectResult({ kind: 'no-model' })
      return
    }
    setDetecting(true)
    try {
      const responses = await testChannel(channelId, {
        model,
        endpoint_type: 'openai-response',
      })
      if (responses.success) {
        setDetectResult({ kind: 'responses-ok', model, time: responses.time })
        return
      }
      const chat = await testChannel(channelId, {
        model,
        endpoint_type: 'openai',
      })
      setDetectResult(
        chat.success
          ? { kind: 'suggest-chat', model, error: responses.message }
          : { kind: 'failed', model, error: responses.message }
      )
    } catch (error) {
      setDetectResult({
        kind: 'failed',
        model,
        error: error instanceof Error ? error.message : String(error),
      })
    } finally {
      setDetecting(false)
    }
  }

  return (
    <div className='flex flex-col gap-3'>
      <FormField
        control={form.control}
        name='twork_runtime'
        render={({ field }) => (
          <FormItem>
            <FormLabel>{t('Twork Runtime')}</FormLabel>
            <FormControl>
              <ToggleGroup
                value={[field.value ?? 'legacy']}
                onValueChange={(value) => {
                  const next = value.find((item) => item !== field.value)
                  if (next !== 'legacy' && next !== 'omp') return
                  field.onChange(next)
                  if (next === 'omp' && !wires.includes(wire as TworkOMPWire)) {
                    form.setValue('twork_wire_api', wires[0], {
                      shouldDirty: true,
                    })
                  }
                }}
                aria-label={t('Twork Runtime')}
                variant='outline'
                className='w-fit'
              >
                <ToggleGroupItem value='legacy' className={RUNTIME_TOGGLE_ITEM}>
                  {t('Legacy')}
                </ToggleGroupItem>
                <ToggleGroupItem value='omp' className={RUNTIME_TOGGLE_ITEM}>
                  {t('New OMP')}
                </ToggleGroupItem>
              </ToggleGroup>
            </FormControl>
            <FormDescription className='text-xs'>
              {t(
                'New OMP channels serve only new Twork clients; old clients never use them.'
              )}
            </FormDescription>
          </FormItem>
        )}
      />

      {runtime === 'omp' && initialRuntime === 'legacy' && channelId && (
        <Alert>
          <CircleAlert aria-hidden='true' />
          <AlertDescription>
            {t(
              'After saving, old Twork clients will stop using this channel. Do not switch channels that old clients still rely on.'
            )}
          </AlertDescription>
        </Alert>
      )}

      {runtime === 'omp' && (
        <div className='bg-muted/40 flex flex-col gap-2 rounded-lg border p-3'>
          <div className='flex items-center justify-between gap-2'>
            <Label>{t('Request Protocol')}</Label>
            {wires.length > 1 && (
              <Button
                type='button'
                variant='outline'
                size='sm'
                disabled={!channelId || detecting}
                onClick={detect}
              >
                <RefreshCw
                  className={cn('h-3.5 w-3.5', detecting && 'animate-spin')}
                  aria-hidden='true'
                />
                {t('Detect')}
              </Button>
            )}
          </div>
          {wires.length === 1 ? (
            <div className='text-muted-foreground rounded-md border border-dashed px-3 py-2 text-sm'>
              {t('{{protocol}} (fixed for this channel type)', {
                protocol: TWORK_OMP_WIRE_LABELS[wires[0]],
              })}
            </div>
          ) : (
            <FormField
              control={form.control}
              name='twork_wire_api'
              render={({ field }) => (
                <FormItem>
                  <FormControl>
                    <RadioGroup
                      value={field.value}
                      onValueChange={(value) => field.onChange(value)}
                      className='grid gap-2'
                    >
                      {wires.map((option) => (
                        <Label
                          key={option}
                          htmlFor={`twork-wire-${option}`}
                          className={cn(
                            'bg-background flex cursor-pointer items-start gap-2.5 rounded-md border px-3 py-2',
                            field.value === option && 'border-foreground'
                          )}
                        >
                          <RadioGroupItem
                            id={`twork-wire-${option}`}
                            value={option}
                            className='mt-0.5'
                          />
                          <span className='flex flex-col gap-0.5'>
                            <span className='text-sm font-medium'>
                              {TWORK_OMP_WIRE_LABELS[option]}
                            </span>
                            <span className='text-muted-foreground text-xs font-normal'>
                              {t(WIRE_HINTS[option])}
                            </span>
                          </span>
                        </Label>
                      ))}
                    </RadioGroup>
                  </FormControl>
                </FormItem>
              )}
            />
          )}
          {wires.length > 1 && (
            <p className='text-muted-foreground text-xs'>
              {channelId
                ? t(
                    'Detect sends a tiny request using the saved channel config and the test model (or the first model); it only checks basic connectivity.'
                  )
                : t('Save the channel first, then detect while editing.')}
            </p>
          )}
          {detectResult && (
            <DetectResultNote
              result={detectResult}
              onUseChat={() =>
                form.setValue('twork_wire_api', 'chat-completions', {
                  shouldDirty: true,
                })
              }
            />
          )}
        </div>
      )}
    </div>
  )
}

function DetectResultNote(props: {
  result: DetectResult
  onUseChat: () => void
}) {
  const { t } = useTranslation()
  const { result } = props
  if (result.kind === 'no-model') {
    return (
      <p className='text-warning text-xs'>
        {t('Add at least one model before detecting.')}
      </p>
    )
  }
  if (result.kind === 'responses-ok') {
    return (
      <p className='text-success flex items-center gap-1.5 text-xs'>
        <CircleCheck className='h-3.5 w-3.5' aria-hidden='true' />
        {t('Responses is available ({{model}}{{time}})', {
          model: result.model,
          time: result.time ? ` · ${Math.round(result.time * 1000)} ms` : '',
        })}
      </p>
    )
  }
  if (result.kind === 'suggest-chat') {
    return (
      <div className='text-warning flex flex-wrap items-center gap-x-2 gap-y-1 text-xs'>
        <CircleAlert className='h-3.5 w-3.5' aria-hidden='true' />
        <span>
          {t(
            'Responses is unavailable, Chat Completions works. Switching is up to you.'
          )}
        </span>
        <Button
          type='button'
          variant='link'
          size='sm'
          className='h-auto p-0 text-xs'
          onClick={props.onUseChat}
        >
          {t('Use Chat Completions')}
        </Button>
      </div>
    )
  }
  return (
    <p className='text-destructive flex items-start gap-1.5 text-xs'>
      <CircleAlert className='mt-0.5 h-3.5 w-3.5 shrink-0' aria-hidden='true' />
      <span>
        {t('Both protocols failed ({{model}}): {{error}}', {
          model: result.model,
          error: result.error || '-',
        })}
      </span>
    </p>
  )
}

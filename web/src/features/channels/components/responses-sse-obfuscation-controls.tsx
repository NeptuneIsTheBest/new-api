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
import { useWatch, type Control } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import {
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
} from '@/components/ui/form'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'

import type { ChannelFormValues } from '../lib'

type ResponsesSSEObfuscationControlsProps = {
  control: Control<ChannelFormValues>
}

export function ResponsesSSEObfuscationControls(
  props: ResponsesSSEObfuscationControlsProps
) {
  const { t } = useTranslation()
  const forceIncludeObfuscation = useWatch({
    control: props.control,
    name: 'force_include_obfuscation',
  })

  return (
    <>
      <FormField
        control={props.control}
        name='force_include_obfuscation'
        render={({ field }) => {
          let selectedValue = 'default'
          if (field.value === true) {
            selectedValue = 'enabled'
          } else if (field.value === false) {
            selectedValue = 'disabled'
          }

          return (
            <FormItem className='flex items-center justify-between gap-3 px-4 py-3'>
              <div className='space-y-0.5'>
                <FormLabel className='text-sm'>
                  {t('Responses SSE obfuscation')}
                </FormLabel>
                <FormDescription>
                  {t(
                    'Force stream_options.include_obfuscation for upstream Responses SSE requests. Forced values take precedence over passthrough and parameter overrides.'
                  )}
                </FormDescription>
              </div>
              <Select
                items={[
                  {
                    value: 'default',
                    label: t('Do not override'),
                  },
                  {
                    value: 'enabled',
                    label: t('Force enabled'),
                  },
                  {
                    value: 'disabled',
                    label: t('Force disabled'),
                  },
                ]}
                value={selectedValue}
                onValueChange={(value) => {
                  if (value === 'default') {
                    field.onChange(undefined)
                    return
                  }
                  field.onChange(value === 'enabled')
                }}
              >
                <FormControl>
                  <SelectTrigger className='w-40'>
                    <SelectValue />
                  </SelectTrigger>
                </FormControl>
                <SelectContent alignItemWithTrigger={false}>
                  <SelectGroup>
                    <SelectItem value='default'>
                      {t('Do not override')}
                    </SelectItem>
                    <SelectItem value='enabled'>
                      {t('Force enabled')}
                    </SelectItem>
                    <SelectItem value='disabled'>
                      {t('Force disabled')}
                    </SelectItem>
                  </SelectGroup>
                </SelectContent>
              </Select>
            </FormItem>
          )
        }}
      />

      <FormField
        control={props.control}
        name='allow_include_obfuscation'
        render={({ field }) => (
          <FormItem className='flex items-center justify-between gap-3 px-4 py-3'>
            <div className='space-y-0.5'>
              <FormLabel className='text-sm'>
                {t('Allow include usage obfuscation passthrough')}
              </FormLabel>
              <FormDescription>
                {forceIncludeObfuscation !== undefined
                  ? t(
                      'Ignored while a forced Responses SSE obfuscation value is selected'
                    )
                  : t('Pass through the include field for usage obfuscation')}
              </FormDescription>
            </div>
            <FormControl>
              <Switch
                checked={field.value}
                onCheckedChange={field.onChange}
                disabled={forceIncludeObfuscation !== undefined}
              />
            </FormControl>
          </FormItem>
        )}
      />
    </>
  )
}

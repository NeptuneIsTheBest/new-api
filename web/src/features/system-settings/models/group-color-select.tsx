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
import { useTranslation } from 'react-i18next'

import { GroupBadge } from '@/components/group-badge'
import { StatusBadge } from '@/components/status-badge'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { GROUP_COLORS, isGroupColor, type GroupColor } from '@/lib/group-colors'

type GroupColorSelectProps = {
  group: string
  value: GroupColor | null
  onValueChange: (color: GroupColor | null) => void
}

export function GroupColorSelect(props: GroupColorSelectProps) {
  const { t } = useTranslation()
  const labels: Record<GroupColor, string> = {
    blue: t('Blue'),
    green: t('Green'),
    cyan: t('Cyan'),
    purple: t('Purple'),
    pink: t('Pink'),
    red: t('Red'),
    orange: t('Orange'),
    yellow: t('Yellow'),
    grey: t('Grey'),
  }
  const specialGroup = !props.group || props.group === 'auto'
  const items = [
    { value: 'automatic', label: t('Automatic color') },
    ...GROUP_COLORS.map((color) => ({ value: color, label: labels[color] })),
  ]

  return (
    <div className='flex min-w-36 flex-col gap-1'>
      <Select
        items={items}
        value={props.value ?? 'automatic'}
        disabled={specialGroup}
        onValueChange={(value) => {
          if (value === 'automatic') props.onValueChange(null)
          else if (isGroupColor(value)) props.onValueChange(value)
        }}
      >
        <SelectTrigger
          aria-label={t('Color for {{group}}', { group: props.group })}
        >
          <SelectValue />
        </SelectTrigger>
        <SelectContent alignItemWithTrigger={false}>
          <SelectGroup>
            <SelectItem value='automatic'>{t('Automatic color')}</SelectItem>
            {GROUP_COLORS.map((color) => (
              <SelectItem key={color} value={color}>
                <StatusBadge
                  label={labels[color]}
                  color={`var(--group-color-${color})`}
                  copyable={false}
                  showDot
                />
              </SelectItem>
            ))}
          </SelectGroup>
        </SelectContent>
      </Select>
      <GroupBadge group={props.group} color={props.value} />
    </div>
  )
}

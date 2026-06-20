/*
Copyright (C) 2025 QuantumNous

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

import React, { useCallback, useEffect, useRef, useState } from 'react';
import {
  Modal,
  Button,
  Progress,
  Typography,
  Spin,
  Tag,
  Descriptions,
  Collapse,
  Select,
} from '@douyinfe/semi-ui';
import { API, showError, showSuccess } from '../../../../helpers';
import { MOBILE_BREAKPOINT } from '../../../../hooks/common/useIsMobile';

const { Text } = Typography;

const clampPercent = (value) => {
  const v = Number(value);
  if (!Number.isFinite(v)) return 0;
  return Math.max(0, Math.min(100, v));
};

const pickStrokeColor = (percent) => {
  const p = clampPercent(percent);
  if (p >= 95) return '#ef4444';
  if (p >= 80) return '#f59e0b';
  return '#3b82f6';
};

const normalizePlanType = (value) => {
  if (value == null) return '';
  return String(value).trim().toLowerCase();
};

const getWindowDurationSeconds = (windowData) => {
  const value = Number(windowData?.limit_window_seconds);
  if (!Number.isFinite(value) || value <= 0) return null;
  return value;
};

const classifyWindowByDuration = (windowData) => {
  const seconds = getWindowDurationSeconds(windowData);
  if (seconds == null) return null;
  return seconds >= 24 * 60 * 60 ? 'weekly' : 'fiveHour';
};

const resolveRateLimitWindows = (data) => {
  const rateLimit = data?.rate_limit ?? {};
  const primary = rateLimit?.primary_window ?? null;
  const secondary = rateLimit?.secondary_window ?? null;
  const windows = [primary, secondary].filter(Boolean);
  const planType = normalizePlanType(data?.plan_type ?? rateLimit?.plan_type);

  let fiveHourWindow = null;
  let weeklyWindow = null;

  for (const windowData of windows) {
    const bucket = classifyWindowByDuration(windowData);
    if (bucket === 'fiveHour' && !fiveHourWindow) {
      fiveHourWindow = windowData;
      continue;
    }
    if (bucket === 'weekly' && !weeklyWindow) {
      weeklyWindow = windowData;
    }
  }

  if (planType === 'free') {
    if (!weeklyWindow) {
      weeklyWindow = primary ?? secondary ?? null;
    }
    return { fiveHourWindow: null, weeklyWindow };
  }

  if (!fiveHourWindow && !weeklyWindow) {
    return {
      fiveHourWindow: primary ?? null,
      weeklyWindow: secondary ?? null,
    };
  }

  if (!fiveHourWindow) {
    fiveHourWindow =
      windows.find((windowData) => windowData !== weeklyWindow) ?? null;
  }
  if (!weeklyWindow) {
    weeklyWindow =
      windows.find((windowData) => windowData !== fiveHourWindow) ?? null;
  }

  return { fiveHourWindow, weeklyWindow };
};

const formatDurationSeconds = (seconds, t) => {
  const tt = typeof t === 'function' ? t : (v) => v;
  const s = Number(seconds);
  if (!Number.isFinite(s) || s <= 0) return '-';
  const total = Math.floor(s);
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const secs = total % 60;
  if (hours > 0) return `${hours}${tt('小时')} ${minutes}${tt('分钟')}`;
  if (minutes > 0) return `${minutes}${tt('分钟')} ${secs}${tt('秒')}`;
  return `${secs}${tt('秒')}`;
};

const formatUnixSeconds = (unixSeconds) => {
  const v = Number(unixSeconds);
  if (!Number.isFinite(v) || v <= 0) return '-';
  try {
    return new Date(v * 1000).toLocaleString();
  } catch (error) {
    return String(unixSeconds);
  }
};

const getDisplayText = (value) => {
  if (value == null) return '';
  return String(value).trim();
};

const formatISODateTime = (value) => {
  const text = getDisplayText(value);
  if (!text) return '-';
  const date = new Date(text);
  if (Number.isNaN(date.getTime())) return '-';
  const pad = (v) => String(v).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(
    date.getDate(),
  )} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(
    date.getSeconds(),
  )}`;
};

const humanizeRawValue = (value) =>
  getDisplayText(value)
    .split(/[_\s-]+/)
    .filter(Boolean)
    .map((part) => part.slice(0, 1).toUpperCase() + part.slice(1))
    .join(' ');

const getResetCreditID = (credit) => getDisplayText(credit?.id);

const getResetCreditTitle = (credit, t) => {
  const tt = typeof t === 'function' ? t : (v) => v;
  return (
    getDisplayText(credit?.title) || getResetCreditID(credit) || tt('重置额度')
  );
};

const getResetCreditDescription = (credit, t) => {
  const tt = typeof t === 'function' ? t : (v) => v;
  return getDisplayText(credit?.description) || tt('暂无描述');
};

const getResetCreditStatus = (credit) => getDisplayText(credit?.status);

const getResetCreditResetType = (credit) => getDisplayText(credit?.reset_type);

const getResetCreditProfileUserID = (credit) =>
  getDisplayText(credit?.profile_user_id);

const getResetCreditProfileImageURL = (credit) =>
  getDisplayText(credit?.profile_image_url);

const getResetCreditAvatarFallback = (credit, t) =>
  getResetCreditTitle(credit, t).slice(0, 1).toUpperCase() || 'R';

const canRedeemResetCredit = (credit) => {
  if (!credit) return false;
  return (
    getDisplayText(credit.redeemed_at) === '' &&
    ['', 'available'].includes(getResetCreditStatus(credit).toLowerCase())
  );
};

const getResetCreditStatusTag = (credit, t) => {
  const tt = typeof t === 'function' ? t : (v) => v;
  const status = getResetCreditStatus(credit);
  const normalized = status.toLowerCase();
  if (normalized === 'available') {
    return <Tag color='green'>{tt('可用')}</Tag>;
  }
  if (normalized === 'redeemed') {
    return <Tag color='grey'>{tt('已兑换')}</Tag>;
  }
  if (normalized === 'expired') {
    return <Tag color='amber'>{tt('已过期')}</Tag>;
  }
  if (normalized === 'redeeming' || normalized === 'in_progress') {
    return <Tag color='blue'>{tt('兑换中')}</Tag>;
  }
  if (normalized === 'unavailable') {
    return <Tag color='grey'>{tt('不可用')}</Tag>;
  }
  return (
    <Tag color='grey'>{status ? humanizeRawValue(status) : tt('未知状态')}</Tag>
  );
};

const getResetCreditsPayload = (response) => {
  const raw = response?.data;
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return null;
  return raw;
};

const getConsumeResponseData = (response) => {
  const raw = response?.data;
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return null;
  return raw;
};

const formatConsumeFailureMessage = (response, t) => {
  const tt = typeof t === 'function' ? t : (v) => v;
  const data = getConsumeResponseData(response);
  const code = getDisplayText(data?.code);
  const message =
    getDisplayText(data?.message) || getDisplayText(response?.message);

  if (code === 'already_redeemed') {
    return tt('此重置额度已被兑换。');
  }
  if (code === 'no_credit') {
    return tt('此账号没有可用的重置额度。');
  }
  if (code === 'nothing_to_reset') {
    return tt('当前没有可重置的 Codex 额度限制。');
  }
  if (message && code) return `${message} (${code})`;
  return message || code || tt('重置 Codex 额度失败');
};

const createRedeemRequestID = () => {
  if (
    typeof crypto !== 'undefined' &&
    typeof crypto.randomUUID === 'function'
  ) {
    return crypto.randomUUID();
  }
  return undefined;
};

const isMobileViewport = () =>
  typeof window !== 'undefined' && window.innerWidth < MOBILE_BREAKPOINT;

const getCodexUsageModalLayout = () => {
  if (isMobileViewport()) {
    return {
      width: 'calc(100vw - 16px)',
      style: {
        top: 8,
        maxWidth: 'calc(100vw - 16px)',
        margin: '0 auto',
      },
      bodyStyle: {
        maxHeight: 'calc(100vh - 148px)',
        overflowY: 'auto',
        padding: '16px 16px 12px',
      },
    };
  }

  return {
    width: 900,
    style: {
      top: 24,
      maxWidth: 'min(900px, 92vw)',
    },
    bodyStyle: {
      maxHeight: 'calc(100vh - 172px)',
      overflowY: 'auto',
      padding: '20px 24px 16px',
    },
  };
};

const formatAccountTypeLabel = (value, t) => {
  const tt = typeof t === 'function' ? t : (v) => v;
  const normalized = normalizePlanType(value);
  switch (normalized) {
    case 'free':
      return 'Free';
    case 'plus':
      return 'Plus';
    case 'prolite':
      return 'Pro 5x';
    case 'pro':
      return 'Pro 20x';
    case 'team':
      return 'Team';
    case 'enterprise':
      return 'Enterprise';
    default:
      return getDisplayText(value) || tt('未识别');
  }
};

const getAccountTypeTagColor = (value) => {
  const normalized = normalizePlanType(value);
  switch (normalized) {
    case 'enterprise':
      return 'green';
    case 'team':
      return 'cyan';
    case 'prolite':
    case 'pro':
      return 'blue';
    case 'plus':
      return 'violet';
    case 'free':
      return 'amber';
    default:
      return 'grey';
  }
};

const resolveUsageStatusTag = (t, rateLimit) => {
  const tt = typeof t === 'function' ? t : (v) => v;
  if (!rateLimit || Object.keys(rateLimit).length === 0) {
    return <Tag color='grey'>{tt('待确认')}</Tag>;
  }
  if (rateLimit?.allowed && !rateLimit?.limit_reached) {
    return <Tag color='green'>{tt('可用')}</Tag>;
  }
  return <Tag color='red'>{tt('受限')}</Tag>;
};

const AccountInfoValue = ({ t, value, onCopy, monospace = false }) => {
  const tt = typeof t === 'function' ? t : (v) => v;
  const text = getDisplayText(value);
  const hasValue = text !== '';

  return (
    <div className='flex min-w-0 items-start justify-between gap-2'>
      <div
        className={`min-w-0 flex-1 break-all text-xs leading-5 text-semi-color-text-1 ${
          monospace ? 'font-mono' : ''
        }`}
      >
        {hasValue ? text : '-'}
      </div>
      <Button
        size='small'
        type='tertiary'
        theme='borderless'
        className='shrink-0 px-1 text-xs'
        disabled={!hasValue}
        onClick={() => onCopy?.(text)}
      >
        {tt('复制')}
      </Button>
    </div>
  );
};

const RateLimitWindowCard = ({ t, title, windowData }) => {
  const tt = typeof t === 'function' ? t : (v) => v;
  const hasWindowData =
    !!windowData &&
    typeof windowData === 'object' &&
    Object.keys(windowData).length > 0;
  const percent = clampPercent(windowData?.used_percent ?? 0);
  const resetAt = windowData?.reset_at;
  const resetAfterSeconds = windowData?.reset_after_seconds;
  const limitWindowSeconds = windowData?.limit_window_seconds;

  return (
    <div className='rounded-lg border border-semi-color-border bg-semi-color-bg-0 p-3'>
      <div className='flex flex-wrap items-start justify-between gap-x-3 gap-y-1'>
        <div className='font-medium'>{title}</div>
        <Text type='tertiary' size='small'>
          {tt('重置时间：')}
          {formatUnixSeconds(resetAt)}
        </Text>
      </div>

      {hasWindowData ? (
        <div className='mt-2'>
          <Progress
            percent={percent}
            stroke={pickStrokeColor(percent)}
            showInfo={true}
          />
        </div>
      ) : (
        <div className='mt-3 text-sm text-semi-color-text-2'>-</div>
      )}

      <div className='mt-1 flex flex-wrap items-center gap-2 text-xs text-semi-color-text-2'>
        <div>
          {tt('已使用：')}
          {hasWindowData ? `${percent}%` : '-'}
        </div>
        <div>
          {tt('距离重置：')}
          {hasWindowData ? formatDurationSeconds(resetAfterSeconds, tt) : '-'}
        </div>
        <div>
          {tt('窗口：')}
          {hasWindowData ? formatDurationSeconds(limitWindowSeconds, tt) : '-'}
        </div>
      </div>
    </div>
  );
};

const RateLimitWindowGrid = ({ t, fiveHourWindow, weeklyWindow }) => {
  const tt = typeof t === 'function' ? t : (v) => v;

  return (
    <div className='grid grid-cols-1 gap-3 md:grid-cols-2'>
      <RateLimitWindowCard
        t={tt}
        title={tt('5小时窗口')}
        windowData={fiveHourWindow}
      />
      <RateLimitWindowCard
        t={tt}
        title={tt('每周窗口')}
        windowData={weeklyWindow}
      />
    </div>
  );
};

const RateLimitGroupSection = ({
  t,
  title,
  description,
  rateLimitSource,
  statusTag,
  meteredFeature,
}) => {
  const tt = typeof t === 'function' ? t : (v) => v;
  const { fiveHourWindow, weeklyWindow } =
    resolveRateLimitWindows(rateLimitSource);
  const featureText = getDisplayText(meteredFeature);

  return (
    <section className='space-y-3'>
      <div className='flex flex-wrap items-start justify-between gap-3'>
        <div className='min-w-0 space-y-2'>
          <div className='flex flex-wrap items-center gap-2'>
            <div className='text-sm font-semibold text-semi-color-text-0'>
              {title}
            </div>
            {statusTag}
          </div>
          {(description || featureText) && (
            <div className='flex flex-wrap items-center gap-2 text-xs text-semi-color-text-2'>
              {description ? <span>{description}</span> : null}
              {featureText ? (
                <div className='inline-flex max-w-full items-center gap-2 rounded-full bg-semi-color-fill-0 px-2 py-1'>
                  <span className='text-[11px] text-semi-color-text-2'>
                    metered_feature
                  </span>
                  <span className='min-w-0 break-all font-mono text-xs text-semi-color-text-0'>
                    {featureText}
                  </span>
                </div>
              ) : null}
            </div>
          )}
        </div>
      </div>

      <RateLimitWindowGrid
        t={tt}
        fiveHourWindow={fiveHourWindow}
        weeklyWindow={weeklyWindow}
      />
    </section>
  );
};

const ResetCreditField = ({ label, value, monospace = false }) => (
  <div className='flex min-w-0 items-center gap-2 text-xs leading-5 text-semi-color-text-2'>
    <span className='shrink-0'>{label}</span>
    <span
      className={`min-w-0 truncate text-semi-color-text-0 ${
        monospace ? 'font-mono' : ''
      }`}
      title={value}
    >
      {value || '-'}
    </span>
  </div>
);

const ResetCreditCard = ({ t, credit, selected }) => {
  const tt = typeof t === 'function' ? t : (v) => v;
  const title = getResetCreditTitle(credit, tt);
  const profileImageURL = getResetCreditProfileImageURL(credit);
  const profileUserID = getResetCreditProfileUserID(credit);
  const status = getResetCreditStatus(credit);

  return (
    <div
      className={`rounded-lg border border-semi-color-border px-3 py-2 ${
        selected ? 'bg-semi-color-fill-0' : 'bg-semi-color-bg-0'
      }`}
    >
      <div className='flex min-w-0 items-start gap-3'>
        {profileImageURL ? (
          <img
            src={profileImageURL}
            alt={title}
            className='mt-0.5 h-7 w-7 shrink-0 rounded-full object-cover'
            onError={(event) => {
              event.currentTarget.style.display = 'none';
            }}
          />
        ) : (
          <div className='mt-0.5 flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-semi-color-fill-0 text-xs font-semibold text-semi-color-text-2'>
            {getResetCreditAvatarFallback(credit, tt)}
          </div>
        )}
        <div className='min-w-0 flex-1'>
          <div className='flex flex-wrap items-center gap-2'>
            <div className='min-w-0 flex-1 truncate text-sm font-semibold text-semi-color-text-0'>
              {title}
            </div>
            {getResetCreditStatusTag(credit, tt)}
          </div>
          <div className='mt-1 text-xs leading-5 text-semi-color-text-2'>
            {getResetCreditDescription(credit, tt)}
          </div>
          <div className='mt-2 grid grid-cols-1 gap-1 md:grid-cols-2'>
            <ResetCreditField
              label={tt('重置额度 ID：')}
              value={getResetCreditID(credit)}
              monospace={true}
            />
            <ResetCreditField
              label={tt('重置类型：')}
              value={getResetCreditResetType(credit)}
              monospace={true}
            />
            <ResetCreditField
              label={tt('状态：')}
              value={status ? humanizeRawValue(status) : '-'}
            />
            <ResetCreditField
              label={tt('发放时间：')}
              value={formatISODateTime(credit?.granted_at)}
            />
            <ResetCreditField
              label={tt('过期时间：')}
              value={formatISODateTime(credit?.expires_at)}
            />
            <ResetCreditField
              label={tt('兑换开始时间：')}
              value={formatISODateTime(credit?.redeem_started_at)}
            />
            <ResetCreditField
              label={tt('兑换时间：')}
              value={formatISODateTime(credit?.redeemed_at)}
            />
            {profileUserID ? (
              <ResetCreditField
                label={tt('Profile 用户 ID：')}
                value={profileUserID}
                monospace={true}
              />
            ) : null}
          </div>
        </div>
      </div>
    </div>
  );
};

const CodexResetCreditsPanel = ({
  t,
  response,
  loading,
  error,
  selectedResetCreditId,
  onSelectedResetCreditIdChange,
  consuming,
  consumeNotice,
  onConsume,
  onRefresh,
}) => {
  const tt = typeof t === 'function' ? t : (v) => v;
  const payload = getResetCreditsPayload(response);
  const resetCredits = Array.isArray(payload?.credits)
    ? payload.credits.filter((credit) => getResetCreditID(credit) !== '')
    : [];
  const redeemableResetCredits = resetCredits.filter(canRedeemResetCredit);
  const availableCount = Number.isFinite(Number(payload?.available_count))
    ? Number(payload?.available_count)
    : redeemableResetCredits.length;
  const activeSelectedResetCreditId = redeemableResetCredits.some(
    (credit) => getResetCreditID(credit) === selectedResetCreditId,
  )
    ? selectedResetCreditId
    : getResetCreditID(redeemableResetCredits[0]);
  const selectedResetCredit = resetCredits.find(
    (credit) => getResetCreditID(credit) === activeSelectedResetCreditId,
  );

  useEffect(() => {
    if (
      activeSelectedResetCreditId &&
      activeSelectedResetCreditId !== selectedResetCreditId
    ) {
      onSelectedResetCreditIdChange(activeSelectedResetCreditId);
    }
  }, [
    activeSelectedResetCreditId,
    onSelectedResetCreditIdChange,
    selectedResetCreditId,
  ]);

  return (
    <div className='rounded-xl border border-semi-color-border bg-semi-color-bg-0 p-3'>
      <div className='flex flex-wrap items-start justify-between gap-3'>
        <div className='min-w-0'>
          <div className='text-sm font-semibold text-semi-color-text-0'>
            {tt('Codex 重置额度')}
          </div>
          <Text type='tertiary' size='small'>
            {tt('使用可用的重置额度重置当前 Codex 额度。')}
          </Text>
        </div>
        <div className='flex flex-wrap items-center gap-2'>
          <Tag color={availableCount > 0 ? 'green' : 'grey'} type='light'>
            {tt('可用数量：')}
            {availableCount}
          </Tag>
          <Button
            size='small'
            type='tertiary'
            theme='outline'
            onClick={onRefresh}
            loading={loading}
          >
            {tt('刷新')}
          </Button>
        </div>
      </div>

      {error ? (
        <div className='mt-3 rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700'>
          {error}
        </div>
      ) : null}

      {consumeNotice ? (
        <div
          className={`mt-3 rounded-lg border px-3 py-2 text-sm ${
            consumeNotice.type === 'success'
              ? 'border-semi-color-success-light-default bg-semi-color-success-light-default text-semi-color-success'
              : 'border-red-200 bg-red-50 text-red-700'
          }`}
        >
          {consumeNotice.message}
        </div>
      ) : null}

      {loading ? (
        <div className='mt-4 flex items-center justify-center py-4'>
          <div className='inline-flex items-center gap-2 whitespace-nowrap text-sm text-semi-color-text-2'>
            <Spin spinning={true} size='small' />
            <span>{tt('正在加载重置额度...')}</span>
          </div>
        </div>
      ) : resetCredits.length === 0 ? (
        <div className='mt-3 text-sm text-semi-color-text-2'>
          {tt('暂无可用重置额度。')}
        </div>
      ) : (
        <div className='mt-3 flex flex-col gap-3'>
          {redeemableResetCredits.length > 0 ? (
            <div className='flex flex-col gap-2 sm:flex-row sm:items-center'>
              <Select
                className='w-full sm:w-[280px]'
                value={activeSelectedResetCreditId}
                placeholder={tt('选择重置额度')}
                optionList={redeemableResetCredits.map((credit) => ({
                  value: getResetCreditID(credit),
                  label: getResetCreditTitle(credit, tt),
                }))}
                onChange={(value) => {
                  onSelectedResetCreditIdChange(getDisplayText(value));
                }}
              />
              <Button
                size='small'
                type='primary'
                theme='solid'
                loading={consuming}
                disabled={!activeSelectedResetCreditId || consuming}
                onClick={() => {
                  const creditId = activeSelectedResetCreditId;
                  if (!creditId || consuming) return;
                  Modal.confirm({
                    title: tt('重置 Codex 额度'),
                    content: tt('使用可用的重置额度重置当前 Codex 额度。'),
                    onOk: () => onConsume?.(creditId),
                    size: 'small',
                    centered: true,
                  });
                }}
              >
                {tt('重置 Codex 额度')}
              </Button>
            </div>
          ) : (
            <div className='text-sm text-semi-color-text-2'>
              {tt('没有可兑换的重置额度。')}
            </div>
          )}

          <div className='flex max-h-80 flex-col gap-2 overflow-y-auto pr-1'>
            {resetCredits.map((credit) => {
              const creditId = getResetCreditID(credit);
              return (
                <ResetCreditCard
                  key={creditId}
                  t={tt}
                  credit={credit}
                  selected={
                    creditId ===
                    (selectedResetCredit
                      ? getResetCreditID(selectedResetCredit)
                      : '')
                  }
                />
              );
            })}
          </div>
        </div>
      )}
    </div>
  );
};

const CodexUsageView = ({
  t,
  record,
  payload,
  onCopy,
  onRefresh,
  resetCreditsResponse,
  resetCreditsLoading,
  resetCreditsError,
  selectedResetCreditId,
  onSelectedResetCreditIdChange,
  consumingResetCredit,
  consumeNotice,
  onRefreshResetCredits,
  onConsumeResetCredit,
}) => {
  const tt = typeof t === 'function' ? t : (v) => v;
  const [showRawJson, setShowRawJson] = useState(false);
  const data = payload?.data ?? null;
  const rateLimit = data?.rate_limit ?? {};
  const additionalRateLimits = Array.isArray(data?.additional_rate_limits)
    ? data.additional_rate_limits.filter(
        (item) =>
          item && typeof item === 'object' && Object.keys(item).length > 0,
      )
    : [];
  const upstreamStatus = payload?.upstream_status;
  const accountType = data?.plan_type ?? rateLimit?.plan_type;
  const accountTypeLabel = formatAccountTypeLabel(accountType, tt);
  const accountTypeTagColor = getAccountTypeTagColor(accountType);
  const statusTag = resolveUsageStatusTag(tt, rateLimit);
  const userId = data?.user_id;
  const email = data?.email;
  const accountId = data?.account_id;
  const errorMessage =
    payload?.success === false
      ? getDisplayText(payload?.message) || tt('获取用量失败')
      : '';

  const rawText =
    typeof data === 'string' ? data : JSON.stringify(data ?? payload, null, 2);

  return (
    <div className='flex flex-col gap-4'>
      {errorMessage && (
        <div className='rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700'>
          {errorMessage}
        </div>
      )}

      <div className='rounded-xl border border-semi-color-border bg-semi-color-bg-0 p-3'>
        <div className='flex flex-wrap items-start justify-between gap-2'>
          <div className='min-w-0'>
            <div className='text-xs font-medium text-semi-color-text-2'>
              {tt('Codex 帐号')}
            </div>
            <div className='mt-2 flex flex-wrap items-center gap-2'>
              <Tag
                color={accountTypeTagColor}
                type='light'
                shape='circle'
                size='large'
                className='font-semibold'
              >
                {accountTypeLabel}
              </Tag>
              {statusTag}
              <Tag color='grey' type='light' shape='circle'>
                {tt('上游状态码：')}
                {upstreamStatus ?? '-'}
              </Tag>
            </div>
          </div>
          <Button
            size='small'
            type='tertiary'
            theme='outline'
            onClick={onRefresh}
          >
            {tt('刷新')}
          </Button>
        </div>

        <div className='mt-2 rounded-lg bg-semi-color-fill-0 px-3 py-2'>
          <Descriptions>
            <Descriptions.Item itemKey='User ID'>
              <AccountInfoValue
                t={tt}
                value={userId}
                onCopy={onCopy}
                monospace={true}
              />
            </Descriptions.Item>
            <Descriptions.Item itemKey={tt('邮箱')}>
              <AccountInfoValue t={tt} value={email} onCopy={onCopy} />
            </Descriptions.Item>
            <Descriptions.Item itemKey='Account ID'>
              <AccountInfoValue
                t={tt}
                value={accountId}
                onCopy={onCopy}
                monospace={true}
              />
            </Descriptions.Item>
          </Descriptions>
        </div>

        <div className='mt-2 text-xs text-semi-color-text-2'>
          {tt('渠道：')}
          {record?.name || '-'} ({tt('编号：')}
          {record?.id || '-'})
        </div>
      </div>

      <div>
        <div className='mb-2'>
          <div className='text-sm font-semibold text-semi-color-text-0'>
            {tt('额度窗口')}
          </div>
          <Text type='tertiary' size='small'>
            {tt(
              '用于观察当前帐号在 Codex 上游的基础限额与附加计费能力使用情况',
            )}
          </Text>
        </div>
      </div>

      <div className='space-y-5'>
        <RateLimitGroupSection
          t={tt}
          title={tt('基础额度')}
          description={tt('当前帐号的基础额度窗口')}
          rateLimitSource={data}
          statusTag={statusTag}
        />

        {additionalRateLimits.length > 0 ? (
          <div className='space-y-4 border-t border-semi-color-border pt-4'>
            <div>
              <div className='text-sm font-semibold text-semi-color-text-0'>
                {tt('附加额度')}
              </div>
              <Text type='tertiary' size='small'>
                {tt('按模型或能力拆分的附加计费能力窗口')}
              </Text>
            </div>

            <div className='space-y-4'>
              {additionalRateLimits.map((item, index) => {
                const limitName =
                  getDisplayText(item?.limit_name) ||
                  getDisplayText(item?.metered_feature) ||
                  `${tt('附加额度')} ${index + 1}`;

                return (
                  <div
                    key={`${limitName}-${getDisplayText(item?.metered_feature)}-${index}`}
                    className={
                      index > 0 ? 'border-t border-semi-color-border pt-4' : ''
                    }
                  >
                    <RateLimitGroupSection
                      t={tt}
                      title={limitName}
                      description={tt('附加计费能力')}
                      rateLimitSource={item}
                      statusTag={resolveUsageStatusTag(tt, item?.rate_limit)}
                      meteredFeature={item?.metered_feature}
                    />
                  </div>
                );
              })}
            </div>
          </div>
        ) : null}
      </div>

      <CodexResetCreditsPanel
        t={tt}
        response={resetCreditsResponse}
        loading={resetCreditsLoading}
        error={resetCreditsError}
        selectedResetCreditId={selectedResetCreditId}
        onSelectedResetCreditIdChange={onSelectedResetCreditIdChange}
        consuming={consumingResetCredit}
        consumeNotice={consumeNotice}
        onRefresh={onRefreshResetCredits}
        onConsume={onConsumeResetCredit}
      />

      <Collapse
        activeKey={showRawJson ? ['raw-json'] : []}
        onChange={(activeKey) => {
          const keys = Array.isArray(activeKey) ? activeKey : [activeKey];
          setShowRawJson(keys.includes('raw-json'));
        }}
      >
        <Collapse.Panel header={tt('原始 JSON')} itemKey='raw-json'>
          <div className='mb-2 flex justify-end'>
            <Button
              size='small'
              type='primary'
              theme='outline'
              onClick={() => onCopy?.(rawText)}
              disabled={!rawText}
            >
              {tt('复制')}
            </Button>
          </div>
          <pre className='max-h-[50vh] overflow-y-auto rounded-lg bg-semi-color-fill-0 p-3 text-xs text-semi-color-text-0'>
            {rawText}
          </pre>
        </Collapse.Panel>
      </Collapse>
    </div>
  );
};

const CodexUsageLoader = ({ t, record, initialPayload, onCopy }) => {
  const tt = typeof t === 'function' ? t : (v) => v;
  const [loading, setLoading] = useState(!initialPayload);
  const [payload, setPayload] = useState(initialPayload ?? null);
  const [resetCreditsResponse, setResetCreditsResponse] = useState(null);
  const [resetCreditsLoading, setResetCreditsLoading] = useState(false);
  const [resetCreditsError, setResetCreditsError] = useState('');
  const [selectedResetCreditId, setSelectedResetCreditId] = useState('');
  const [consumingResetCredit, setConsumingResetCredit] = useState(false);
  const [consumeNotice, setConsumeNotice] = useState(null);
  const hasShownErrorRef = useRef(false);
  const hasShownResetCreditsErrorRef = useRef(false);
  const mountedRef = useRef(true);
  const resetCreditsRequestRef = useRef(0);
  const consumeResetCreditRequestRef = useRef(0);
  const recordId = record?.id;

  const fetchUsage = useCallback(async () => {
    if (!recordId) {
      if (mountedRef.current) setPayload(null);
      return;
    }

    if (mountedRef.current) setLoading(true);
    try {
      const res = await API.get(`/api/channel/${recordId}/codex/usage`, {
        skipErrorHandler: true,
      });
      if (!mountedRef.current) return;
      setPayload(res?.data ?? null);
      if (!res?.data?.success && !hasShownErrorRef.current) {
        hasShownErrorRef.current = true;
        showError(tt('获取用量失败'));
      }
    } catch (error) {
      if (!mountedRef.current) return;
      if (!hasShownErrorRef.current) {
        hasShownErrorRef.current = true;
        showError(tt('获取用量失败'));
      }
      setPayload({ success: false, message: String(error) });
    } finally {
      if (mountedRef.current) setLoading(false);
    }
  }, [recordId, tt]);

  const fetchResetCredits = useCallback(async () => {
    if (!recordId) {
      if (mountedRef.current) {
        setResetCreditsResponse(null);
        setResetCreditsError('');
      }
      return;
    }

    const requestId = resetCreditsRequestRef.current + 1;
    resetCreditsRequestRef.current = requestId;

    if (mountedRef.current) {
      setResetCreditsLoading(true);
      setResetCreditsError('');
    }

    try {
      const res = await API.get(
        `/api/channel/${recordId}/codex/rate-limit-reset-credits`,
        {
          skipErrorHandler: true,
          disableDuplicate: true,
        },
      );
      if (!mountedRef.current || resetCreditsRequestRef.current !== requestId) {
        return;
      }

      const response = res?.data ?? null;
      setResetCreditsResponse(response);
      if (!response?.success) {
        const message =
          getDisplayText(response?.message) || tt('获取重置额度失败');
        setResetCreditsError(message);
        if (!hasShownResetCreditsErrorRef.current) {
          hasShownResetCreditsErrorRef.current = true;
          showError(message);
        }
      }
    } catch (error) {
      if (!mountedRef.current || resetCreditsRequestRef.current !== requestId) {
        return;
      }
      const message =
        error instanceof Error ? error.message : tt('获取重置额度失败');
      setResetCreditsResponse(null);
      setResetCreditsError(message);
      if (!hasShownResetCreditsErrorRef.current) {
        hasShownResetCreditsErrorRef.current = true;
        showError(message);
      }
    } finally {
      if (mountedRef.current && resetCreditsRequestRef.current === requestId) {
        setResetCreditsLoading(false);
      }
    }
  }, [recordId, tt]);

  const handleConsumeResetCredit = useCallback(
    async (creditId) => {
      const requestedCreditId = getDisplayText(creditId);
      if (!recordId || !requestedCreditId) return;

      const requestId = consumeResetCreditRequestRef.current + 1;
      consumeResetCreditRequestRef.current = requestId;

      if (mountedRef.current) {
        setConsumingResetCredit(true);
        setConsumeNotice(null);
      }

      try {
        const res = await API.post(
          `/api/channel/${recordId}/codex/rate-limit-reset-credits/consume`,
          {
            credit_id: requestedCreditId,
            redeem_request_id: createRedeemRequestID(),
          },
          { skipErrorHandler: true },
        );
        if (
          !mountedRef.current ||
          consumeResetCreditRequestRef.current !== requestId
        ) {
          return;
        }

        const response = res?.data ?? {};
        const data = getConsumeResponseData(response);
        const code = getDisplayText(data?.code);
        if (code === 'reset') {
          const message = tt('Codex 额度已重置。');
          setConsumeNotice({ type: 'success', message });
          showSuccess(message);
          await Promise.all([fetchUsage(), fetchResetCredits()]);
          return;
        }

        const message = formatConsumeFailureMessage(response, tt);
        setConsumeNotice({ type: 'error', message });
        showError(message);
      } catch (error) {
        if (
          !mountedRef.current ||
          consumeResetCreditRequestRef.current !== requestId
        ) {
          return;
        }
        const message =
          error instanceof Error ? error.message : tt('重置 Codex 额度失败');
        setConsumeNotice({ type: 'error', message });
        showError(message);
      } finally {
        if (
          mountedRef.current &&
          consumeResetCreditRequestRef.current === requestId
        ) {
          setConsumingResetCredit(false);
        }
      }
    },
    [fetchResetCredits, fetchUsage, recordId, tt],
  );

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      resetCreditsRequestRef.current += 1;
      consumeResetCreditRequestRef.current += 1;
    };
  }, []);

  useEffect(() => {
    setResetCreditsResponse(null);
    setResetCreditsError('');
    setSelectedResetCreditId('');
    setConsumeNotice(null);
    setConsumingResetCredit(false);
    hasShownResetCreditsErrorRef.current = false;

    if (!initialPayload) {
      fetchUsage().catch(() => {});
    }
    fetchResetCredits().catch(() => {});
  }, [fetchResetCredits, fetchUsage, initialPayload, recordId]);

  if (loading) {
    return (
      <div className='flex items-center justify-center py-10'>
        <Spin spinning={true} size='large' tip={tt('加载中...')} />
      </div>
    );
  }

  if (!payload) {
    return (
      <div className='flex flex-col gap-3'>
        <Text type='danger'>{tt('获取用量失败')}</Text>
        <div className='flex justify-end'>
          <Button
            size='small'
            type='primary'
            theme='outline'
            onClick={fetchUsage}
          >
            {tt('刷新')}
          </Button>
        </div>
      </div>
    );
  }

  return (
    <CodexUsageView
      t={tt}
      record={record}
      payload={payload}
      onCopy={onCopy}
      onRefresh={fetchUsage}
      resetCreditsResponse={resetCreditsResponse}
      resetCreditsLoading={resetCreditsLoading}
      resetCreditsError={resetCreditsError}
      selectedResetCreditId={selectedResetCreditId}
      onSelectedResetCreditIdChange={setSelectedResetCreditId}
      consumingResetCredit={consumingResetCredit}
      consumeNotice={consumeNotice}
      onRefreshResetCredits={fetchResetCredits}
      onConsumeResetCredit={handleConsumeResetCredit}
    />
  );
};

export const openCodexUsageModal = ({ t, record, payload, onCopy }) => {
  const tt = typeof t === 'function' ? t : (v) => v;
  const layout = getCodexUsageModalLayout();

  Modal.info({
    title: tt('Codex 帐号与用量'),
    centered: false,
    width: layout.width,
    style: layout.style,
    bodyStyle: layout.bodyStyle,
    content: (
      <CodexUsageLoader
        t={tt}
        record={record}
        initialPayload={payload}
        onCopy={onCopy}
      />
    ),
    footer: (
      <div className='flex justify-end gap-2'>
        <Button type='primary' theme='solid' onClick={() => Modal.destroyAll()}>
          {tt('关闭')}
        </Button>
      </div>
    ),
  });
};

import { useState, type FormEvent, type ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowDown, ArrowUp, Plus, Trash2 } from 'lucide-react'
import { toast } from 'sonner'

import { api } from '@/lib/api'
import {
  createClaudeAccount, deleteClaudeAccount, listClaudeAccounts, toggleClaudeAccount,
} from '@/lib/claudeAccounts'
import { fetchServerSettings, restartServer, updateServerSettings } from '@/lib/settings'
import type { ClaudeAccount } from '@/lib/types'
import { useAuth } from '@/stores/auth'
import { useTheme } from '@/stores/theme'

export interface AccountPolicy { mode: 'priority' | 'balanced'; order: string[]; throttled?: string[] }
interface Usage { today: number; week: number }

export const fetchPolicy = () => api<AccountPolicy>('/api/v1/claude-accounts/policy')
const savePolicy = (p: AccountPolicy) =>
  api<AccountPolicy>('/api/v1/claude-accounts/policy', { method: 'PUT', body: { mode: p.mode, order: p.order } })
const fetchUsage = () =>
  api<{ accounts: Record<string, Usage> }>('/api/v1/chat-usage').then((r) => r.accounts ?? {})

export const FONT_SIZES = [
  { px: 14, label: 'کوچک' },
  { px: 15, label: 'معمولی' },
  { px: 17, label: 'بزرگ' },
  { px: 19, label: 'خیلی بزرگ' },
]

const num = (n: number) => n.toLocaleString('fa-IR')
const errText = (e: unknown) => (e instanceof Error ? e.message : undefined)

export function orderedAccounts(list: ClaudeAccount[], order: string[]) {
  const rank = (a: ClaudeAccount) => {
    const i = order.indexOf(a.id)
    return i < 0 ? order.length : i
  }
  return [...list].sort((a, b) => rank(a) - rank(b) || a.name.localeCompare(b.name))
}

function Section({ title, hint, children }: { title: string; hint?: string; children: ReactNode }) {
  return (
    <section className="cl-set">
      <h3>{title}</h3>
      {hint ? <p className="cl-set-hint">{hint}</p> : null}
      {children}
    </section>
  )
}

function Seg<T extends string | number>({
  value, options, onChange,
}: { value: T; options: { value: T; label: string }[]; onChange: (v: T) => void }) {
  return (
    <div className="cl-seg">
      {options.map((o) => (
        <button key={String(o.value)} className={o.value === value ? 'on' : ''} onClick={() => onChange(o.value)}>{o.label}</button>
      ))}
    </div>
  )
}

function Accounts() {
  const qc = useQueryClient()
  const accountsQ = useQuery({ queryKey: ['claude-accounts'], queryFn: listClaudeAccounts })
  const policyQ = useQuery({ queryKey: ['account-policy'], queryFn: fetchPolicy })
  const usageQ = useQuery({ queryKey: ['chat-usage'], queryFn: fetchUsage, refetchInterval: 30000 })
  const settingsQ = useQuery({ queryKey: ['server-settings'], queryFn: fetchServerSettings })
  const [name, setName] = useState('')
  const [token, setToken] = useState('')
  const [needRestart, setNeedRestart] = useState(false)

  const policy: AccountPolicy = policyQ.data ?? { mode: 'balanced', order: [] }
  const list = orderedAccounts(accountsQ.data ?? [], policy.order)
  const usage = usageQ.data ?? {}
  const refresh = () => qc.invalidateQueries({ queryKey: ['claude-accounts'] })

  const policyMut = useMutation({
    mutationFn: savePolicy,
    onMutate: (p) => { qc.setQueryData(['account-policy'], { ...policy, ...p }) },
    onError: (e) => toast.error('ذخیره نشد', { description: errText(e) }),
  })
  const move = (id: string, dir: -1 | 1) => {
    const ids = list.map((a) => a.id)
    const i = ids.indexOf(id)
    const j = i + dir
    if (j < 0 || j >= ids.length) return
    ;[ids[i], ids[j]] = [ids[j], ids[i]]
    policyMut.mutate({ ...policy, order: ids })
  }
  const add = useMutation({
    mutationFn: () => createClaudeAccount({ name: name.trim(), token: token.trim() }),
    onSuccess: () => {
      setName('')
      setToken('')
      toast.success('اکانت اضافه شد')
      void refresh()
    },
    onError: (e) => toast.error('اکانت اضافه نشد', { description: errText(e) }),
  })
  const toggle = useMutation({
    mutationFn: (a: ClaudeAccount) => toggleClaudeAccount(a.id, !a.enabled),
    onSuccess: () => void refresh(),
    onError: (e) => toast.error('تغییر نکرد', { description: errText(e) }),
  })
  const remove = async (a: ClaudeAccount) => {
    if (!window.confirm(`اکانت «${a.name}» و توکن آن حذف شود؟`)) return
    try {
      await deleteClaudeAccount(a.id)
      void refresh()
    } catch (e) {
      toast.error('حذف نشد', { description: errText(e) })
    }
  }
  const failover = settingsQ.data?.config.providers.claude.auto_failover_enabled === true
  const setFailover = async (v: boolean) => {
    const cfg = settingsQ.data?.config
    if (!cfg) return
    try {
      await updateServerSettings({
        ...cfg,
        providers: { ...cfg.providers, claude: { ...cfg.providers.claude, auto_failover_enabled: v } },
      })
      setNeedRestart(true)
      await qc.invalidateQueries({ queryKey: ['server-settings'] })
    } catch (e) {
      toast.error('ذخیره نشد', { description: errText(e) })
    }
  }

  return (
    <>
      <Section title="نحوه‌ی استفاده از اکانت‌ها" hint="برای چت‌هایی که اکانتشان روی «خودکار» است.">
        <Seg
          value={policy.mode}
          options={[{ value: 'priority', label: 'به ترتیب اولویت' }, { value: 'balanced', label: 'توزیع یکسان' }]}
          onChange={(mode) => policyMut.mutate({ mode, order: list.map((a) => a.id) })}
        />
        <p className="cl-set-hint" style={{ marginTop: 8 }}>
          {policy.mode === 'priority'
            ? 'چت جدید روی بالاترین اکانتِ فعال می‌رود و اگر به لیمیت خورده باشد، نوبت بعدی.'
            : 'چت جدید روی اکانتی می‌رود که چت‌های باز کمتری دارد.'}
        </p>
        <label className="cl-switch">
          <input type="checkbox" checked={failover} disabled={!settingsQ.data} onChange={(e) => void setFailover(e.target.checked)} />
          وقتی چتی به لیمیت خورد، خودکار روی اکانت بعدی ادامه پیدا کند
        </label>
        {needRestart ? (
          <div className="cl-banner">
            این تغییر بعد از ری‌استارت opendray اعمال می‌شود؛ بعدش باید دوباره وارد شوید.
            <button onClick={() => void restartServer().then(() => toast.success('در حال ری‌استارت…'))}>ری‌استارت</button>
          </div>
        ) : null}
      </Section>

      <Section title="اکانت‌های Claude" hint="مصرف فقط چت‌هایی را می‌شمارد که از opendray اجرا شده‌اند و به اکانت فعلی همان چت نسبت داده می‌شود. مقدار باقی‌مانده‌ی لیمیت را Claude در اختیار opendray نمی‌گذارد.">
        {list.map((a, i) => {
          const u = usage[a.id] ?? { today: 0, week: 0 }
          const limited = policy.throttled?.includes(a.id)
          return (
            <div className="cl-acc" key={a.id}>
              {policy.mode === 'priority' ? (
                <div className="cl-acc-move">
                  <button aria-label="بالاتر" disabled={i === 0} onClick={() => move(a.id, -1)}><ArrowUp size={14} /></button>
                  <button aria-label="پایین‌تر" disabled={i === list.length - 1} onClick={() => move(a.id, 1)}><ArrowDown size={14} /></button>
                </div>
              ) : null}
              <div className="cl-acc-main">
                <div className="cl-acc-name">
                  <span dir="auto">{a.display_name || a.name}</span>
                  {limited ? <span className="cl-tag">به لیمیت خورده</span> : null}
                  {!a.enabled ? <span className="cl-tag off">غیرفعال</span> : null}
                  {!a.token_filled ? <span className="cl-tag">بدون توکن</span> : null}
                </div>
                <div className="cl-acc-meta">
                  امروز {num(u.today)} توکن · ۷ روز {num(u.week)} توکن · {num(a.active_sessions ?? 0)} چت باز
                </div>
              </div>
              <label className="cl-switch sm">
                <input type="checkbox" checked={a.enabled} aria-label="فعال" onChange={() => toggle.mutate(a)} />
              </label>
              <button className="cl-icon" aria-label="حذف اکانت" onClick={() => void remove(a)}><Trash2 size={15} /></button>
            </div>
          )
        })}
      </Section>

      <Section title="افزودن اکانت جدید" hint="با همان اکانت در ترمینال این دستور را بزنید و توکنی را که می‌دهد اینجا بگذارید:">
        <pre className="cl-cmd" dir="ltr">claude setup-token</pre>
        <form
          className="cl-form"
          onSubmit={(e: FormEvent) => {
            e.preventDefault()
            if (name.trim() && token.trim()) add.mutate()
          }}
        >
          <input dir="auto" placeholder="اسم اکانت (انگلیسی، مثل work)" value={name} pattern="[A-Za-z0-9_\-]+" required onChange={(e) => setName(e.target.value)} />
          <input dir="ltr" type="password" placeholder="sk-ant-oat01-…" autoComplete="off" value={token} required onChange={(e) => setToken(e.target.value)} />
          <button className="cl-btn" disabled={add.isPending}><Plus size={15} />افزودن اکانت</button>
        </form>
      </Section>
    </>
  )
}

function Password() {
  const setSession = useAuth((s) => s.setSession)
  const username = useAuth((s) => s.username)
  const [cur, setCur] = useState('')
  const [next, setNext] = useState('')
  const [again, setAgain] = useState('')
  const change = useMutation({
    mutationFn: () => api<{ token: string; username: string; expires_at: string }>(
      '/api/v1/auth/change-credentials',
      { method: 'POST', body: { current_password: cur, new_user: '', new_password: next }, skipAuthRedirect: true },
    ),
    onSuccess: (r) => {
      if (r?.token) setSession(r.token, r.username, r.expires_at)
      setCur('')
      setNext('')
      setAgain('')
      toast.success('رمز عوض شد')
    },
    onError: (e) => toast.error('رمز عوض نشد', { description: errText(e) }),
  })
  return (
    <Section title="تغییر رمز پنل" hint={username ? `نام کاربری: ${username}` : undefined}>
      <form
        className="cl-form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault()
          if (next !== again) { toast.error('رمز جدید و تکرارش یکی نیستند'); return }
          change.mutate()
        }}
      >
        <input dir="ltr" type="password" placeholder="رمز فعلی" autoComplete="current-password" value={cur} required onChange={(e) => setCur(e.target.value)} />
        <input dir="ltr" type="password" placeholder="رمز جدید" autoComplete="new-password" value={next} required onChange={(e) => setNext(e.target.value)} />
        <input dir="ltr" type="password" placeholder="تکرار رمز جدید" autoComplete="new-password" value={again} required onChange={(e) => setAgain(e.target.value)} />
        <button className="cl-btn" disabled={change.isPending}>تغییر رمز</button>
      </form>
    </Section>
  )
}

export function ChatSettings({ fontSize, onFontSize }: { fontSize: number; onFontSize: (px: number) => void }) {
  const theme = useTheme()
  return (
    <div className="cl-scroll">
      <div className="cl-col">
        <h2 className="cl-page-title">تنظیمات</h2>
        <Section title="ظاهر">
          <div className="cl-set-row">
            <span>اندازه‌ی متن</span>
            <Seg value={fontSize} options={FONT_SIZES.map((f) => ({ value: f.px, label: f.label }))} onChange={onFontSize} />
          </div>
          <div className="cl-set-row">
            <span>حالت نمایش</span>
            <Seg
              value={theme.mode}
              options={[{ value: 'light', label: 'روشن' }, { value: 'dark', label: 'تیره' }, { value: 'system', label: 'مثل سیستم' }]}
              onChange={theme.setMode}
            />
          </div>
        </Section>
        <Accounts />
        <Password />
      </div>
    </div>
  )
}

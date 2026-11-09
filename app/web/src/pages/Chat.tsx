import { useCallback, useEffect, useMemo, useRef, useState, type CSSProperties, type KeyboardEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate, useSearch } from '@tanstack/react-router'
import ReactMarkdown, { type Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import {
  ArrowUp, ChevronDown, ChevronLeft, FileText, Folder, FolderPlus, History, LayoutGrid, Menu,
  MoreHorizontal, Paperclip, Pencil, Plus, Search, Settings, Square, Tag,
  Trash2, Wrench, X,
} from 'lucide-react'
import { toast } from 'sonner'
import '@fontsource-variable/vazirmatn'
import './chat.css'

import { api } from '@/lib/api'
import {
  createSession, listSessions, removeSession, startSession, uploadSessionFile,
} from '@/lib/sessions'
import { listClaudeAccounts } from '@/lib/claudeAccounts'
import { isTerminalSessionState, type Session } from '@/lib/types'
import { ChatSettings, fetchPolicy, orderedAccounts } from './ChatSettings'
import { ChatPanel, PANES, type PaneId } from './ChatPanel'
import { TooltipProvider } from '@/components/ui/tooltip'
import { FileBrowserDialog } from '@/components/sessions/FileBrowserDialog'

interface Block { type: 'text' | 'tool_use' | 'tool_result'; text?: string; name?: string; summary?: string; desc?: string; target?: string; tool_id?: string; is_error?: boolean; ask?: AskQuestion[] }
interface Message { id: string; role: 'user' | 'assistant'; ts: string; blocks: Block[] }
interface AskQuestion { question: string; header?: string; multiSelect?: boolean; options: { label: string; description?: string }[] }
interface ScreenPrompt { title: string; options: string[]; selected: number; trust?: boolean }
interface Conversation { messages: Message[]; working: boolean; pending_tool: boolean; ready: boolean; prompt?: ScreenPrompt; mode?: string; activity?: string; limit_note?: string; in_use_elsewhere?: boolean }
interface OldChat { id: string; title: string; cwd: string; updated: string; in_use?: boolean }
interface Meta { topics: string[]; chats: Record<string, { title?: string; topic?: string }> }

const fetchConversation = (id: string) => api<Conversation>(`/api/v1/sessions/${id}/conversation`)
const fetchMeta = () => api<Meta>('/api/v1/chat-meta')
const saveMeta = (m: Meta) => api<Meta>('/api/v1/chat-meta', { method: 'PUT', body: m })
const sendRaw = (id: string, data: string) =>
  api(`/api/v1/sessions/${id}/input`, { method: 'POST', body: { data } })
const fetchOldChats = () => api<{ chats: OldChat[] }>('/api/v1/old-chats').then((r) => r.chats ?? [])
const importOldChat = (id: string, account: string) =>
  api<Session>(`/api/v1/old-chats/${id}/import`, { method: 'POST', body: { claude_account_id: account } })
const faDate = (s: string) => {
  try {
    return new Intl.DateTimeFormat('fa-IR-u-ca-persian', { day: 'numeric', month: 'long', year: 'numeric' }).format(new Date(s))
  } catch { return '' }
}
// One line per attachment; the bubble shows it as a chip, Claude reads the path.
const ATTACH_RE = /^پیوست: (\S+) \((.*)\)$/
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))
const LS_CWD = 'cl-cwd'
const LS_ACC = 'cl-account'
const LS_FS = 'cl-font-size'
const LS_MODE = 'cl-mode'
const LS_SEEN = 'cl-touched'
const fetchActivity = () =>
  api<{ sessions: Record<string, string>; working: string[] }>('/api/v1/chat-activity')
const readSeen = (): Record<string, number> => { try { return JSON.parse(localStorage.getItem(LS_SEEN) ?? '{}') ?? {} } catch { return {} } }
const lsGet = (k: string) => { try { return localStorage.getItem(k) ?? '' } catch { return '' } }
const lsSet = (k: string, v: string) => { try { localStorage.setItem(k, v) } catch { /* private mode */ } }

async function waitReady(id: string) {
  for (let i = 0; i < 60; i++) {
    const c = await fetchConversation(id).catch(() => null)
    if (c?.ready) return true
    if (c?.prompt) i = 0 // waiting on the operator, not on the CLI
    await sleep(500)
  }
  return false
}

// Bracketed paste keeps newlines inside the prompt; Enter goes as its own
// write because Ink swallows a trailing \r that arrives in the paste burst.
async function submit(id: string, text: string) {
  await sendRaw(id, `\x1b[200~${text}\x1b[201~`)
  await sleep(350)
  await sendRaw(id, '\r')
}

// Moves the TUI menu cursor to the chosen row, then confirms it.
async function answerPrompt(id: string, p: ScreenPrompt, index: number) {
  const key = index > p.selected ? '\x1b[B' : '\x1b[A'
  for (let i = 0; i < Math.abs(index - p.selected); i++) {
    await sendRaw(id, key)
    await sleep(120)
  }
  await sendRaw(id, '\r')
}

// AskForm renders Claude's AskUserQuestion from the transcript. The TUI's
// tabbed dialog is unreadable on screen (and RTL text comes out mirrored),
// so the answers go back as one plain chat message instead.
function AskForm({ questions, onSubmit }: { questions: AskQuestion[]; onSubmit: (text: string) => void }) {
  const [picked, setPicked] = useState<Record<number, string[]>>({})
  const [other, setOther] = useState<Record<number, string>>({})
  const toggle = (qi: number, label: string, multi: boolean) => setPicked((prev) => {
    const cur = prev[qi] ?? []
    const next = multi ? (cur.includes(label) ? cur.filter((l) => l !== label) : [...cur, label]) : (cur[0] === label ? [] : [label])
    return { ...prev, [qi]: next }
  })
  const answerOf = (qi: number) => [...(picked[qi] ?? []), ...((other[qi] ?? '').trim() ? [(other[qi] ?? '').trim()] : [])]
  const done = questions.every((_, qi) => answerOf(qi).length > 0)
  const build = () => questions.map((q, qi) => `${q.header || q.question}: ${answerOf(qi).join('، ')}`).join('\n')
  return (
    <div className="cl-ask">
      {questions.map((q, qi) => (
        <div key={qi} className="cl-ask-q">
          <div className="cl-ask-title" dir="auto">
            {q.header ? <b>{q.header}: </b> : null}{q.question}
            {q.multiSelect ? <span className="cl-ask-hint"> (چند گزینه)</span> : null}
          </div>
          <div className="cl-ask-opts">
            {q.options.map((o) => {
              const on = (picked[qi] ?? []).includes(o.label)
              return (
                <button key={o.label} dir="auto" data-on={on ? 1 : 0} onClick={() => toggle(qi, o.label, !!q.multiSelect)}>
                  <div>{on ? '✓ ' : ''}{o.label}</div>
                  {o.description ? <div className="cl-ask-desc">{o.description}</div> : null}
                </button>
              )
            })}
            <input
              className="cl-ask-other" dir="auto" placeholder="جواب دیگر…"
              value={other[qi] ?? ''} onChange={(e) => setOther((p) => ({ ...p, [qi]: e.target.value }))}
            />
          </div>
        </div>
      ))}
      <button className="cl-ask-send" disabled={!done} onClick={() => onSubmit(build())}>ارسال جواب‌ها</button>
    </div>
  )
}

const MODES = [
  { value: 'auto', label: 'Auto: تصمیم با Claude' },
  { value: 'manual', label: 'Manual: همیشه بپرس' },
  { value: 'acceptEdits', label: 'پذیرش ویرایش‌ها' },
  { value: 'plan', label: 'Plan: فقط برنامه‌ریزی' },
]

interface NextStep { recap?: string; ask?: string; actions?: string[] }
const NEXT_STEP_RE = /<next_step>([\s\S]*?)<\/next_step>\s*$/
const stripNextStep = (t: string) => t.replace(NEXT_STEP_RE, '').trim()
function readNextStep(t: string): NextStep | null {
  const m = NEXT_STEP_RE.exec(t)
  if (!m) return null
  try { return JSON.parse(m[1]) as NextStep } catch { return null }
}

function NextStepCard({ step, onPick, onClose }: { step: NextStep; onPick: (a: string) => void; onClose: () => void }) {
  const mac = /Mac|iPhone|iPad/.test(navigator.platform)
  return (
    <div className="cl-next">
      <button className="cl-next-x" aria-label="بستن" onClick={onClose}><X size={16} /></button>
      {step.recap ? <div className="cl-next-recap" dir="auto">{step.recap}</div> : null}
      <div className="cl-next-ask" dir="auto">{step.ask}</div>
      <div className="cl-next-actions">
        {(step.actions ?? []).map((a, i) => (
          <button key={a} className={i === 0 ? 'primary' : ''} dir="auto" onClick={() => onPick(a)}>
            {a}{i === 0 ? <kbd dir="ltr">{mac ? '⌘' : 'Ctrl'} ↵</kbd> : null}
          </button>
        ))}
      </div>
    </div>
  )
}

const mdComponents: Components = {
  p: ({ children }) => <p dir="auto">{children}</p>,
  li: ({ children }) => <li dir="auto">{children}</li>,
  h1: ({ children }) => <h1 dir="auto">{children}</h1>,
  h2: ({ children }) => <h2 dir="auto">{children}</h2>,
  h3: ({ children }) => <h3 dir="auto">{children}</h3>,
  h4: ({ children }) => <h4 dir="auto">{children}</h4>,
  blockquote: ({ children }) => <blockquote dir="auto">{children}</blockquote>,
  table: ({ children }) => <div className="cl-table"><table dir="auto">{children}</table></div>,
  a: ({ children, href }) => <a href={href} target="_blank" rel="noreferrer">{children}</a>,
}

const baseName = (p?: string) => (p ?? '').split('/').filter(Boolean).pop() ?? ''

// Plain-language line for a tool call; the raw command stays in the details.
function toolLabel(b: Block): string {
  const file = baseName(b.target)
  const name = b.name ?? ''
  switch (name) {
    case 'Bash': return b.desc || 'اجرای یک دستور'
    case 'Read': return `خواندن ${file || 'فایل'}`
    case 'Write': return `نوشتن ${file || 'فایل'}`
    case 'Edit':
    case 'MultiEdit':
    case 'NotebookEdit': return `ویرایش ${file || 'فایل'}`
    case 'Grep': return b.target ? `جستجو برای «${b.target}»` : 'جستجو در فایل‌ها'
    case 'Glob': return 'پیدا کردن فایل‌ها'
    case 'WebSearch': return b.target ? `جستجوی وب: ${b.target}` : 'جستجو در وب'
    case 'WebFetch': return 'خواندن یک صفحه‌ی وب'
    case 'Agent':
    case 'Task': return b.desc || 'انجام یک کار فرعی'
    case 'TodoWrite': return 'به‌روزرسانی فهرست کارها'
    case 'ToolSearch': return 'آماده کردن ابزارها'
    case 'Skill': return 'استفاده از یک مهارت'
  }
  if (name.includes('chrome') || name.includes('Browser')) return 'کار با مرورگر'
  if (name.startsWith('mcp__')) return b.desc || `استفاده از ابزار ${name.split('__').pop()}`
  return b.desc || name
}

function ToolRow({ block, result }: { block: Block; result?: Block }) {
  return (
    <details className={`cl-tool${result?.is_error ? ' err' : ''}`}>
      <summary>
        <Wrench size={13} style={{ flex: 'none' }} />
        <span className="cl-tool-line" dir="auto">{toolLabel(block)}</span>
      </summary>
      <pre>{block.summary || block.name}{result?.text ? `\n\n${result.text}` : ''}</pre>
    </details>
  )
}

function UserBubble({ text }: { text: string }) {
  const lines = text.split('\n')
  const files = lines.map((l) => ATTACH_RE.exec(l)).filter((m) => m !== null)
  const body = lines.filter((l) => !ATTACH_RE.test(l)).join('\n').trim()
  return (
    <div dir="auto">
      {files.length ? (
        <div className="cl-files">
          {files.map((m, i) => <span className="cl-file" key={i} title={m[1]}><FileText size={14} />{m[2]}</span>)}
        </div>
      ) : null}
      {body}
    </div>
  )
}

function Thread({ messages, pending }: { messages: Message[]; pending: string | null }) {
  const results = useMemo(() => {
    const m = new Map<string, Block>()
    for (const msg of messages) for (const b of msg.blocks) if (b.type === 'tool_result' && b.tool_id) m.set(b.tool_id, b)
    return m
  }, [messages])
  return (
    <>
      {messages.map((msg) => {
        const text = msg.blocks.filter((b) => b.type === 'text')
        if (msg.role === 'user') {
          if (!text.length) return null
          return (
            <div className="cl-user" key={msg.id}>
              <UserBubble text={text.map((b) => b.text).join('\n')} />
            </div>
          )
        }
        return (
          <div className="cl-assistant" key={msg.id}>
            {msg.blocks.map((b, i) =>
              b.type === 'text' ? (
                <div className="cl-md" key={i}>
                  <ReactMarkdown remarkPlugins={[remarkGfm]} components={mdComponents}>{stripNextStep(b.text ?? '')}</ReactMarkdown>
                </div>
              ) : b.type === 'tool_use' ? (
                <ToolRow key={i} block={b} result={b.tool_id ? results.get(b.tool_id) : undefined} />
              ) : null,
            )}
          </div>
        )
      })}
      {pending ? (
        <div className="cl-user cl-pending"><UserBubble text={pending} /></div>
      ) : null}
    </>
  )
}

function Composer({
  value, onChange, onSend, onStop, busy, autoFocus, children, files, onFiles,
}: {
  value: string; onChange: (v: string) => void; onSend: () => void; onStop?: () => void
  busy: boolean; autoFocus?: boolean; children?: React.ReactNode
  files: File[]; onFiles: (f: File[]) => void
}) {
  const pick = useRef<HTMLInputElement>(null)
  const [drag, setDrag] = useState(false)
  const add = (list: FileList | File[] | null) => {
    const next = Array.from(list ?? [])
    if (next.length) onFiles([...files, ...next])
  }
  const canSend = !!value.trim() || files.length > 0
  const ref = useRef<HTMLTextAreaElement>(null)
  useEffect(() => {
    const el = ref.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = `${Math.min(el.scrollHeight, 220)}px`
  }, [value])
  const onKey = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault()
      onSend()
    }
  }
  return (
    <div className="cl-compose-wrap">
      <div
        className={`cl-compose${drag ? ' drag' : ''}`}
        onDragOver={(e) => { e.preventDefault(); setDrag(true) }}
        onDragLeave={() => setDrag(false)}
        onDrop={(e) => { e.preventDefault(); setDrag(false); add(e.dataTransfer.files) }}
      >
        {files.length ? (
          <div className="cl-files">
            {files.map((f, i) => (
              <span className="cl-file" key={`${f.name}-${i}`}>
                <FileText size={14} />{f.name}
                <button aria-label="حذف فایل" onClick={() => onFiles(files.filter((_, j) => j !== i))}><X size={12} /></button>
              </span>
            ))}
          </div>
        ) : null}
        <textarea
          ref={ref} rows={1} dir={value ? "auto" : "rtl"} value={value} autoFocus={autoFocus}
          placeholder="پیام خود را بنویسید…"
          onChange={(e) => onChange(e.target.value)} onKeyDown={onKey}
          onPaste={(e) => {
            if (e.clipboardData.files.length) { e.preventDefault(); add(e.clipboardData.files) }
          }}
        />
        <div className="cl-compose-row">
          <input
            ref={pick} type="file" multiple hidden
            onChange={(e) => { add(e.target.files); e.target.value = '' }}
          />
          <button className="cl-icon" onClick={() => pick.current?.click()} title="پیوست فایل" aria-label="پیوست فایل"><Paperclip size={17} /></button>
          {children}
          {busy && onStop && !canSend ? (
            <button className="cl-send" onClick={onStop} title="توقف"><Square size={14} fill="currentColor" /></button>
          ) : (
            <button className="cl-send" disabled={!canSend} onClick={onSend} title="ارسال"><ArrowUp size={18} /></button>
          )}
        </div>
      </div>
    </div>
  )
}

function ChatRow({
  title, active, working, topics, onOpen, onRename, onMove, onDelete,
}: {
  title: string; active: boolean; working: boolean; topics: string[]
  onOpen: () => void; onRename: (t: string) => void; onMove: (t: string) => void; onDelete: () => void
}) {
  const [menu, setMenu] = useState(false)
  const [edit, setEdit] = useState<string | null>(null)
  useEffect(() => {
    if (!menu) return
    const off = () => setMenu(false)
    window.addEventListener('click', off)
    return () => window.removeEventListener('click', off)
  }, [menu])
  if (edit !== null) {
    return (
      <div className="cl-item" style={{ padding: 3 }}>
        <input
          className="cl-inline-input" autoFocus dir="auto" value={edit}
          onChange={(e) => setEdit(e.target.value)}
          onBlur={() => { if (edit.trim()) onRename(edit.trim()); setEdit(null) }}
          onKeyDown={(e) => {
            if (e.key === 'Enter') e.currentTarget.blur()
            if (e.key === 'Escape') setEdit(null)
          }}
        />
      </div>
    )
  }
  return (
    <div className={`cl-item${active ? ' on' : ''}`}>
      {working ? <span className="cl-dot" title="در حال کار" /> : null}
      <button className="cl-item-btn" dir="auto" onClick={onOpen} title={title}>{title}</button>
      <button
        className="cl-item-more" data-open={menu ? 1 : 0} aria-label="گزینه‌ها"
        onClick={(e) => { e.stopPropagation(); setMenu((v) => !v) }}
      ><MoreHorizontal size={15} /></button>
      {menu ? (
        <div className="cl-menu" onClick={(e) => e.stopPropagation()}>
          <button onClick={() => { setMenu(false); setEdit(title) }}><Pencil size={14} />تغییر نام</button>
          {topics.length ? <div className="cl-menu-label">انتقال به موضوع</div> : null}
          {topics.map((t) => (
            <button key={t} onClick={() => { setMenu(false); onMove(t) }}><Tag size={14} />{t}</button>
          ))}
          {topics.length ? <button onClick={() => { setMenu(false); onMove('') }}><X size={14} />بدون موضوع</button> : null}
          <button className="cl-danger" onClick={() => { setMenu(false); onDelete() }}><Trash2 size={14} />حذف چت</button>
        </div>
      ) : null}
    </div>
  )
}

function OldChatsView({ onOpen, hide }: { onOpen: (c: OldChat) => void; hide: Set<string> }) {
  const q = useQuery({ queryKey: ['old-chats'], queryFn: fetchOldChats })
  const [term, setTerm] = useState('')
  const needle = term.trim().toLowerCase()
  const list = (q.data ?? []).filter((c) => !hide.has(c.id) && (!needle || c.title.toLowerCase().includes(needle)))
  return (
    <div className="cl-scroll">
      <div className="cl-col">
        <h2 className="cl-page-title">گفتگوهای قبلی</h2>
        <label className="cl-search">
          <Search size={16} />
          <input dir={term ? 'auto' : 'rtl'} autoFocus placeholder="جستجو در عنوان گفتگوها" value={term} onChange={(e) => setTerm(e.target.value)} />
        </label>
        <div className="cl-old-count">
          {q.isLoading ? 'در حال خواندن گفتگوها…' : `${list.length.toLocaleString('fa-IR')} گفتگو`}
        </div>
        {list.map((c) => (
          <button className="cl-old" key={c.id} onClick={() => onOpen(c)}>
            <span className="cl-old-title">
              {c.title}{c.in_use ? <span className="cl-tag" style={{ marginInlineStart: 8 }}>در حال استفاده</span> : null}
            </span>
            <span className="cl-old-meta">
              {faDate(c.updated)} · <span dir="ltr">{c.cwd.split('/').pop()}</span>
            </span>
          </button>
        ))}
      </div>
    </div>
  )
}

export function ChatPage() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const search = useSearch({ strict: false }) as { c?: string }
  const activeId = search.c

  const [sideOpen, setSideOpen] = useState(() => window.innerWidth >= 768)
  const [draft, setDraft] = useState('')
  const [pending, setPending] = useState<{ id: string | null; text: string; at: number } | null>(null)
  const [sending, setSending] = useState(false)
  const [files, setFiles] = useState<File[]>([])
  const [startMode, setStartMode] = useState(lsGet(LS_MODE) || 'auto')
  const [closedStep, setClosedStep] = useState('')
  const [view, setView] = useState<'chat' | 'old' | 'settings'>('chat')
  const showOld = view === 'old'
  const [fontSize, setFontSize] = useState(() => Number(lsGet(LS_FS)) || 15)
  const [pane, setPane] = useState<PaneId | null>(null)
  const [picker, setPicker] = useState(false)
  const [newTopic, setNewTopic] = useState<string | null>(null)
  const [newChatTopic, setNewChatTopic] = useState('')
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({})
  const [cwd, setCwd] = useState(lsGet(LS_CWD))
  const [accountId, setAccountId] = useState(lsGet(LS_ACC))

  const sessionsQ = useQuery({ queryKey: ['sessions'], queryFn: listSessions, refetchInterval: 5000 })
  // After opendray restarts with a new build, an open tab would keep
  // running the old code; reload once the server's build changes.
  const buildRef = useRef('')
  useQuery({
    queryKey: ['server-build'],
    queryFn: async () => {
      const r = await fetch('/api/v1/health').then((x) => x.json()).catch(() => null) as { commit?: string } | null
      const c = r?.commit ?? ''
      if (c && buildRef.current && c !== buildRef.current) window.location.reload()
      if (c) buildRef.current = c
      return c
    },
    refetchInterval: 15000,
  })
  const metaQ = useQuery({ queryKey: ['chat-meta'], queryFn: fetchMeta })
  const accountsQ = useQuery({ queryKey: ['claude-accounts'], queryFn: listClaudeAccounts })
  const policyQ = useQuery({ queryKey: ['account-policy'], queryFn: fetchPolicy })
  const meta: Meta = metaQ.data ?? { topics: [], chats: {} }

  const activityQ = useQuery({ queryKey: ['chat-activity'], queryFn: fetchActivity, refetchInterval: 3000 })
  // Last message sent from this browser per chat; opening a chat does
  // not count, only starting it or sending to it.
  const [seen, setSeen] = useState<Record<string, number>>(readSeen)
  const markSeen = useCallback((id: string) => {
    setSeen((prev) => {
      const next = { ...prev, [id]: Date.now() }
      lsSet(LS_SEEN, JSON.stringify(next))
      return next
    })
  }, [])
  // Most recent first: the later of the transcript's last write (either
  // side), the last send from here, and the chat's creation.
  const chats = useMemo(() => {
    const act = activityQ.data?.sessions ?? {}
    const recency = (s: Session) => Math.max(
      Date.parse(act[s.id] ?? '') || 0,
      seen[s.id] ?? 0,
      Date.parse(s.started_at ?? '') || 0,
    )
    return (sessionsQ.data ?? [])
      .filter((s) => s.provider_id === 'claude' && !s.parent_session_id)
      .sort((a, b) => recency(b) - recency(a))
  }, [sessionsQ.data, activityQ.data, seen])
  const workingIds = useMemo(() => new Set(activityQ.data?.working ?? []), [activityQ.data])
  const active = chats.find((s) => s.id === activeId)
  const accounts = useMemo(() => (accountsQ.data ?? []).filter((a) => a.enabled), [accountsQ.data])
  // '' = automatic: the gateway picks by the account policy.
  const effectiveAccount = accounts.some((a) => a.id === accountId) ? accountId : ''
  const autoAccount = () => {
    const policy = policyQ.data
    const free = accounts.filter((a) => !policy?.throttled?.includes(a.id))
    if (policy?.mode === 'priority') return orderedAccounts(free, policy.order)[0]?.id ?? ''
    return [...free].sort((a, b) => (a.active_sessions ?? 0) - (b.active_sessions ?? 0))[0]?.id ?? ''
  }
  const effectiveCwd = cwd || chats[0]?.cwd || ''

  const convQ = useQuery({
    queryKey: ['conversation', activeId],
    queryFn: () => fetchConversation(activeId as string),
    enabled: !!activeId,
    refetchInterval: (q) => {
      if (q.state.data?.working || pending) return 1200
      if (q.state.data?.in_use_elsewhere) return 30000
      return active && isTerminalSessionState(active.state) ? false : 4000
    },
  })
  const conv = convQ.data
  const messages = useMemo(() => conv?.messages ?? [], [conv])

  // The optimistic bubble goes away once the transcript carries it.
  useEffect(() => {
    if (!pending || pending.id !== activeId) return
    // Claude Code may store the text reworded (pasted text wrapped in
    // tags, attachments appended), so match on time, not on wording: any
    // user message recorded after the send is this one.
    const landed = messages.some((m) => m.role === 'user'
      && m.blocks.some((b) => b.type === 'text')
      && Date.parse(m.ts) >= pending.at - 3000)
    const lastUser = [...messages].reverse().find((m) => m.role === 'user' && m.blocks.some((b) => b.type === 'text'))
    const text = lastUser?.blocks.find((b) => b.type === 'text')?.text ?? ''
    if (landed || text.trim() === pending.text.trim()) setPending(null)
  }, [messages, pending, activeId])

  const scrollRef = useRef<HTMLDivElement>(null)
  const stick = useRef(true)
  useEffect(() => { stick.current = true }, [activeId])
  useEffect(() => {
    const el = scrollRef.current
    if (el && stick.current) el.scrollTop = el.scrollHeight
  }, [messages.length, pending, activeId, conv?.working, conv?.prompt?.title, conv?.prompt?.options.length, closedStep])

  const metaMut = useMutation({
    mutationFn: saveMeta,
    onMutate: (m) => { qc.setQueryData(['chat-meta'], m) },
    onError: () => toast.error('ذخیره نشد'),
  })
  const patchChat = useCallback((id: string, patch: { title?: string; topic?: string }) => {
    metaMut.mutate({ ...meta, chats: { ...meta.chats, [id]: { ...meta.chats[id], ...patch } } })
  }, [meta, metaMut])

  const titleOf = (s: Session) => meta.chats[s.id]?.title || s.name || 'چت بدون نام'
  const openChat = (id?: string) => {
    void navigate({ to: '/chat', search: id ? { c: id } : {} })
    setView('chat')
    if (window.innerWidth < 768) setSideOpen(false)
  }

  const send = async (reply?: string) => {
    const typed = (reply ?? draft).trim()
    const attached = reply === undefined ? files : []
    if ((!typed && !attached.length) || sending) return
    if (active && conv?.in_use_elsewhere) {
      toast.error('این گفتگو جای دیگری باز است', { description: 'چند دقیقه بعد از تمام شدن کار آنجا اینجا ادامه بدهید.' })
      return
    }
    const label = typed || 'این فایل را ببین.'
    // The chat needs a session before anything can be uploaded to it.
    const withFiles = async (id: string) => {
      const lines: string[] = []
      for (const f of attached) {
        const r = await uploadSessionFile(id, f)
        lines.push(`پیوست: ${r.path} (${f.name.replace(/\s+/g, ' ')})`)
      }
      return lines.length ? `${label}\n${lines.join('\n')}` : label
    }
    setSending(true)
    if (reply === undefined) {
      setDraft('')
      setFiles([])
    }
    try {
      if (active) {
        const text = await withFiles(active.id)
        setPending({ id: active.id, text, at: Date.now() })
        markSeen(active.id)
        if (isTerminalSessionState(active.state)) {
          await startSession(active.id)
          await qc.invalidateQueries({ queryKey: ['sessions'] })
        }
        if (!(await waitReady(active.id))) {
          setPane('terminal')
          toast.error('Claude آماده نشد؛ ترمینال را ببینید')
        }
        await submit(active.id, text)
      } else {
        if (!effectiveCwd) { setPicker(true); setDraft(typed); setFiles(attached); return }
        const s = await createSession({
          provider_id: 'claude', cwd: effectiveCwd, args: ['--permission-mode', startMode],
          name: label.replace(/\s+/g, ' ').slice(0, 60),
          claude_account_id: effectiveAccount || undefined,
        })
        lsSet(LS_CWD, effectiveCwd)
        if (newChatTopic) patchChat(s.id, { topic: newChatTopic })
        const text = await withFiles(s.id)
        setPending({ id: s.id, text, at: Date.now() })
        markSeen(s.id)
        await qc.invalidateQueries({ queryKey: ['sessions'] })
        void navigate({ to: '/chat', search: { c: s.id } })
        if (!(await waitReady(s.id))) {
          setPane('terminal')
          toast.error('Claude آماده نشد؛ ترمینال را ببینید')
        }
        await submit(s.id, text)
      }
      await qc.invalidateQueries({ queryKey: ['conversation'] })
    } catch (e) {
      setPending(null)
      setDraft(typed)
      setFiles(attached)
      toast.error('ارسال نشد', { description: e instanceof Error ? e.message : undefined })
    } finally {
      setSending(false)
    }
  }

  const openOld = async (c: OldChat) => {
    try {
      const s = await importOldChat(c.id, effectiveAccount || autoAccount())
      await qc.invalidateQueries({ queryKey: ['sessions'] })
      await qc.invalidateQueries({ queryKey: ['old-chats'] })
      openChat(s.id)
    } catch (e) {
      toast.error('باز نشد', { description: e instanceof Error ? e.message : undefined })
    }
  }

  // A live chat changes mode the way the CLI does: Shift+Tab cycles it.
  const changeMode = async (id: string, target: string) => {
    for (let i = 0; i < 6; i++) {
      const c = await fetchConversation(id).catch(() => null)
      if (!c?.ready) { toast.error('الان نمی‌شود حالت را عوض کرد'); return }
      if (c.mode === target) break
      await sendRaw(id, '\x1b[Z')
      await sleep(500)
    }
    const c = await fetchConversation(id).catch(() => null)
    if (c?.mode !== target) toast.error('این حالت برای این چت در دسترس نیست')
    await qc.invalidateQueries({ queryKey: ['conversation', id] })
  }

  const remove = async (s: Session) => {
    if (!window.confirm(`چت «${titleOf(s)}» حذف شود؟`)) return
    try {
      await removeSession(s.id)
      const chatsMeta = { ...meta.chats }
      delete chatsMeta[s.id]
      metaMut.mutate({ ...meta, chats: chatsMeta })
      await qc.invalidateQueries({ queryKey: ['sessions'] })
      if (activeId === s.id) openChat()
    } catch (e) {
      toast.error('حذف نشد', { description: e instanceof Error ? e.message : undefined })
    }
  }

  const addTopic = (name: string) => {
    const t = name.trim()
    setNewTopic(null)
    if (t && !meta.topics.includes(t)) metaMut.mutate({ ...meta, topics: [...meta.topics, t] })
  }
  const deleteTopic = (t: string) => {
    if (!window.confirm(`موضوع «${t}» حذف شود؟ چت‌های آن بدون موضوع می‌شوند.`)) return
    const next: Meta['chats'] = {}
    for (const [k, v] of Object.entries(meta.chats)) next[k] = v.topic === t ? { ...v, topic: '' } : v
    metaMut.mutate({ topics: meta.topics.filter((x) => x !== t), chats: next })
  }

  const row = (s: Session) => (
    <ChatRow
      key={s.id} title={titleOf(s)} active={s.id === activeId} working={workingIds.has(s.id)} topics={meta.topics}
      onOpen={() => openChat(s.id)}
      onRename={(t) => patchChat(s.id, { title: t })}
      onMove={(t) => patchChat(s.id, { topic: t })}
      onDelete={() => void remove(s)}
    />
  )
  const inTopic = (t: string) => chats.filter((s) => (meta.chats[s.id]?.topic ?? '') === t)
  const loose = chats.filter((s) => {
    const t = meta.chats[s.id]?.topic
    return !t || !meta.topics.includes(t)
  })

  const chatPending = pending && pending.id === activeId ? pending.text : null
  const working = !!active && (!!chatPending || !!conv?.working)
  const needsAnswer = !!active && !!conv && !conv.prompt && active.state === 'idle' && (conv.pending_tool || (!conv.ready && !isTerminalSessionState(active.state)))

  // The card belongs to the latest finished turn only.
  const lastMsg = messages[messages.length - 1]
  const pendingAsk = conv?.pending_tool && lastMsg?.role === 'assistant'
    ? [...lastMsg.blocks].reverse().find((b) => b.type === 'tool_use' && b.name === 'AskUserQuestion' && b.ask?.length)?.ask
    : undefined
  const answerAsk = async (text: string) => {
    if (!active) return
    await sendRaw(active.id, '\x1b') // close the TUI dialog
    await sleep(700)
    await send(text)
  }
  const lastText = lastMsg?.role === 'assistant' ? [...lastMsg.blocks].reverse().find((b) => b.type === 'text')?.text : undefined
  const parsedStep = lastText ? readNextStep(lastText) : null
  const nextStep = parsedStep?.ask && !working && !conv?.prompt && lastMsg && lastMsg.id !== closedStep ? parsedStep : null
  const primary = nextStep?.actions?.[0]
  const sendRef = useRef(send)
  sendRef.current = send
  useEffect(() => {
    if (!primary) return
    const onKey = (e: globalThis.KeyboardEvent) => {
      if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
        e.preventDefault()
        void sendRef.current(primary)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [primary])

  const composerExtras = active ? (
    <>
      <span className="cl-chip" dir="ltr" title={active.cwd}><Folder size={13} />{active.cwd.split('/').pop()}</span>
      {conv?.mode ? (
        <select className="cl-chip" value={conv.mode} aria-label="حالت" onChange={(e) => void changeMode(active.id, e.target.value)}>
          {MODES.map((m) => <option key={m.value} value={m.value}>{m.label}</option>)}
          {MODES.some((m) => m.value === conv.mode) ? null : <option value={conv.mode}>{conv.mode}</option>}
        </select>
      ) : null}
    </>
  ) : (
    <>
      <select
        className="cl-chip" value={startMode} aria-label="حالت"
        onChange={(e) => { setStartMode(e.target.value); lsSet(LS_MODE, e.target.value) }}
      >
        {MODES.map((m) => <option key={m.value} value={m.value}>{m.label}</option>)}
      </select>
      <button className="cl-chip" dir="ltr" title={effectiveCwd} onClick={() => setPicker(true)}>
        <Folder size={13} />{effectiveCwd.split('/').pop() || 'انتخاب پوشه'}
      </button>
      {accounts.length > 0 ? (
        <select
          className="cl-chip" value={effectiveAccount} aria-label="اکانت"
          onChange={(e) => { setAccountId(e.target.value); lsSet(LS_ACC, e.target.value) }}
        >
          <option value="">اکانت خودکار</option>
          {accounts.map((a) => <option key={a.id} value={a.id}>{a.display_name || a.name}</option>)}
        </select>
      ) : null}
      {meta.topics.length > 0 ? (
        <select className="cl-chip" value={newChatTopic} aria-label="موضوع" onChange={(e) => setNewChatTopic(e.target.value)}>
          <option value="">بدون موضوع</option>
          {meta.topics.map((t) => <option key={t} value={t}>{t}</option>)}
        </select>
      ) : null}
    </>
  )

  return (
    <TooltipProvider delayDuration={200}>
    <div className="cl-root" dir="rtl" style={{ '--cl-fs': `${fontSize}px` } as CSSProperties}>
      {sideOpen ? <div className="cl-backdrop" onClick={() => setSideOpen(false)} /> : null}
      {sideOpen ? (
        <aside className="cl-side">
          <div className="cl-side-head">
            <div className="cl-brand"><span className="cl-star">✻</span>چت</div>
            <button className="cl-icon" onClick={() => setSideOpen(false)} aria-label="بستن فهرست"><ChevronLeft size={18} style={{ transform: 'scaleX(-1)' }} /></button>
          </div>
          <button className="cl-newchat" onClick={() => openChat()}>
            <span className="cl-plus"><Plus size={16} /></span>چت جدید
          </button>
          <button
            className={`cl-newchat cl-oldbtn${showOld ? ' on' : ''}`}
            onClick={() => { setView('old'); if (window.innerWidth < 768) setSideOpen(false) }}
          >
            <History size={17} />گفتگوهای قبلی
          </button>
          <div className="cl-list">
            {meta.topics.map((t) => (
              <div key={t}>
                <button className="cl-topic" onClick={() => setCollapsed((c) => ({ ...c, [t]: !c[t] }))}>
                  <ChevronDown size={13} style={{ transform: collapsed[t] ? 'rotate(90deg)' : undefined }} />
                  <span className="cl-grow" dir="auto">{t}</span>
                  <span className="cl-topic-act" role="button" title="حذف موضوع" onClick={(e) => { e.stopPropagation(); deleteTopic(t) }}><X size={13} /></span>
                </button>
                {collapsed[t] ? null : inTopic(t).map(row)}
              </div>
            ))}
            {newTopic !== null ? (
              <div style={{ padding: '8px 4px' }}>
                <input
                  className="cl-inline-input" autoFocus dir="auto" placeholder="نام موضوع" value={newTopic}
                  onChange={(e) => setNewTopic(e.target.value)}
                  onBlur={() => addTopic(newTopic)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') e.currentTarget.blur()
                    if (e.key === 'Escape') setNewTopic(null)
                  }}
                />
              </div>
            ) : null}
            {meta.topics.length > 0 && loose.length > 0 ? <div className="cl-topic">چت‌ها</div> : null}
            {meta.topics.length === 0 && loose.length > 0 ? <div className="cl-topic">اخیر</div> : null}
            {loose.map(row)}
          </div>
          <div className="cl-side-foot">
            <button onClick={() => setNewTopic('')}><FolderPlus size={15} />موضوع جدید</button>
            <button onClick={() => { setView('settings'); if (window.innerWidth < 768) setSideOpen(false) }}>
              <Settings size={15} />تنظیمات
            </button>
            <a href={`${import.meta.env.BASE_URL}sessions`}><LayoutGrid size={15} />پنل opendray</a>
          </div>
        </aside>
      ) : null}

      <main className="cl-main">
        <div className="cl-top">
          {!sideOpen ? <button className="cl-icon" onClick={() => setSideOpen(true)} aria-label="باز کردن فهرست"><Menu size={18} /></button> : null}
          <div className="cl-title" dir="auto">{active && view === 'chat' ? titleOf(active) : ''}</div>
          {active && view === 'chat' ? PANES.map((p) => (
            <button
              key={p.id} className={`cl-icon${pane === p.id ? ' on' : ''}`} title={p.label} aria-label={p.label}
              onClick={() => setPane((v) => (v === p.id ? null : p.id))}
            ><p.icon size={17} /></button>
          )) : null}
        </div>

        {view === 'settings' ? (
          <ChatSettings fontSize={fontSize} onFontSize={(px) => { setFontSize(px); lsSet(LS_FS, String(px)) }} />
        ) : showOld ? (
          <OldChatsView
            onOpen={(c) => void openOld(c)}
            hide={new Set(chats.map((s) => s.claude_session_id ?? ''))}
          />
        ) : active ? (
          <>
            <div
              className="cl-scroll" ref={scrollRef}
              onScroll={(e) => {
                const el = e.currentTarget
                stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80
              }}
            >
              <div className="cl-col">
                <Thread messages={messages} pending={chatPending} />
                {pendingAsk ? (
                  <AskForm key={lastMsg?.id} questions={pendingAsk} onSubmit={(t) => void answerAsk(t)} />
                ) : conv?.prompt ? (
                  <div className="cl-ask">
                    <div className="cl-ask-title" dir="auto">
                      {conv.prompt.trust
                        ? 'Claude اولین بار است در این پوشه با این اکانت کار می‌کند. با تأیید، می‌تواند فایل‌های این پوشه را بخواند، تغییر دهد و اجرا کند و دسترسی‌هایی که در تنظیمات این پوشه از قبل تأیید شده‌اند بدون پرسیدن اعمال می‌شوند.'
                        : conv.prompt.title}
                    </div>
                    <div className="cl-ask-opts">
                      {conv.prompt.options.map((o, i) => (
                        <button key={o} dir="auto" onClick={() => void answerPrompt(active.id, conv.prompt as ScreenPrompt, i)}>{o}</button>
                      ))}
                    </div>
                  </div>
                ) : needsAnswer ? (
                  <div className="cl-banner">
                    Claude منتظر جواب شماست.
                    <button onClick={() => setPane('terminal')}>باز کردن ترمینال</button>
                  </div>
                ) : working ? (
                  <div className="cl-thinking">
                    <span className="cl-spin">✻</span>{conv?.pending_tool ? 'در حال اجرای دستور…' : 'در حال فکر کردن…'}
                    {conv?.activity ? <span dir="ltr" className="cl-activity">{conv.activity.replace(/^\S+\s+/, '')}</span> : null}
                  </div>
                ) : nextStep ? (
                  <NextStepCard
                    step={nextStep} onPick={(a) => void send(a)}
                    onClose={() => setClosedStep(lastMsg?.id ?? '')}
                  />
                ) : null}
              </div>
            </div>
            <Composer
              value={draft} onChange={setDraft} onSend={() => void send()} busy={working} autoFocus
              files={files} onFiles={setFiles}
              onStop={() => void sendRaw(active.id, '\x1b')}
            >
              {composerExtras}
            </Composer>
            <div className="cl-hint" style={{ paddingBottom: 8 }}>
              {conv?.limit_note ? <div dir="ltr">{conv.limit_note}</div> : null}
              {conv?.in_use_elsewhere
                ? 'این گفتگو همین حالا جای دیگری باز است؛ فعلاً فقط خواندنی است و ۱۰ دقیقه بعد از آخرین پیام آنجا اینجا هم می‌شود ادامه داد.'
                : isTerminalSessionState(active.state) ? 'این چت متوقف است؛ با ارسال پیام ادامه پیدا می‌کند.' : ''}
            </div>
          </>
        ) : activeId && sessionsQ.isLoading ? null : (
          <div className="cl-hello">
            <h1><span className="cl-star">✻</span>امروز چه کاری انجام بدیم؟</h1>
            <Composer value={draft} onChange={setDraft} onSend={() => void send()} busy={false} autoFocus files={files} onFiles={setFiles}>
              {composerExtras}
            </Composer>
          </div>
        )}
      </main>

      {pane && active && view === 'chat' ? (
        <ChatPanel pane={pane} session={active} onPane={setPane} onClose={() => setPane(null)} />
      ) : null}

      <FileBrowserDialog
        open={picker} onOpenChange={setPicker} initialPath={effectiveCwd || undefined}
        onSelect={(p) => { setCwd(p); lsSet(LS_CWD, p); setPicker(false) }}
      />
    </div>
    </TooltipProvider>
  )
}


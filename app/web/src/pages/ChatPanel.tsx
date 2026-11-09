import { useState, type FormEvent } from 'react'
import { ExternalLink, FolderTree, GitCompare, Globe, RefreshCw, SquareTerminal, X } from 'lucide-react'

import { sessionWorkDir } from '@/lib/sessions'
import { isTerminalSessionState, type Session } from '@/lib/types'
import { FilesPanel } from '@/components/sessions/inspector/FilesPanel'
import { GitPanel } from '@/components/sessions/inspector/GitPanel'
import { Terminal } from '@/components/sessions/Terminal'

export type PaneId = 'files' | 'changes' | 'terminal' | 'web'

export const PANES: { id: PaneId; label: string; icon: typeof Globe }[] = [
  { id: 'files', label: 'فایل‌ها', icon: FolderTree },
  { id: 'changes', label: 'تغییرات', icon: GitCompare },
  { id: 'terminal', label: 'ترمینال', icon: SquareTerminal },
  { id: 'web', label: 'پیش‌نمایش', icon: Globe },
]

const LS_URL = 'cl-preview-url'
const readUrl = () => { try { return localStorage.getItem(LS_URL) || 'http://localhost:3000' } catch { return 'http://localhost:3000' } }

function Preview() {
  const [address, setAddress] = useState(readUrl)
  const [url, setUrl] = useState(readUrl)
  const [nonce, setNonce] = useState(0)
  const go = (e: FormEvent) => {
    e.preventDefault()
    const next = /^https?:\/\//.test(address) ? address : `http://${address}`
    setAddress(next)
    setUrl(next)
    setNonce((n) => n + 1)
    try { localStorage.setItem(LS_URL, next) } catch { /* private mode */ }
  }
  return (
    <div className="cl-web">
      <form className="cl-web-bar" onSubmit={go}>
        <button type="button" className="cl-icon" aria-label="بارگذاری دوباره" onClick={() => setNonce((n) => n + 1)}><RefreshCw size={15} /></button>
        <input value={address} onChange={(e) => setAddress(e.target.value)} spellCheck={false} aria-label="آدرس" />
        <a className="cl-icon" href={url} target="_blank" rel="noreferrer" aria-label="باز کردن در تب تازه"><ExternalLink size={15} /></a>
      </form>
      <iframe key={`${url}#${nonce}`} src={url} title="پیش‌نمایش" />
    </div>
  )
}

export function ChatPanel({ pane, session, onPane, onClose }: {
  pane: PaneId; session: Session; onPane: (p: PaneId) => void; onClose: () => void
}) {
  const cwd = sessionWorkDir(session)
  const live = !isTerminalSessionState(session.state)
  return (
    <aside className="cl-panel" dir="ltr">
      <div className="cl-panel-tabs" dir="rtl">
        {PANES.map((p) => (
          <button key={p.id} className={p.id === pane ? 'on' : ''} onClick={() => onPane(p.id)}>
            <p.icon size={14} />{p.label}
          </button>
        ))}
        <button className="cl-panel-close" aria-label="بستن پنل" onClick={onClose}><X size={15} /></button>
      </div>
      <div className="cl-panel-body">
        {pane === 'files' ? <FilesPanel cwd={cwd} /> : null}
        {pane === 'changes' ? <GitPanel cwd={cwd} /> : null}
        {pane === 'terminal' ? (
          live
            ? <Terminal sessionId={session.id} providerId={session.provider_id} />
            : <div className="cl-panel-empty" dir="rtl">این چت متوقف است؛ با فرستادن پیام ترمینال هم فعال می‌شود.</div>
        ) : null}
        {pane === 'web' ? <Preview /> : null}
      </div>
    </aside>
  )
}

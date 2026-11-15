import { useEffect, useState } from 'react'
import { api, qc, statusColor, statusRu, useAgent, useLive, useTasks } from './api'
import TaskView from './TaskView'

export function Badge({ s }: { s: string }) {
  return <span className={`px-2 py-0.5 rounded text-xs text-white whitespace-nowrap ${statusColor[s] ?? 'bg-zinc-700'}`}>{statusRu[s] ?? s}</span>
}

export function Bar({ v }: { v: number }) {
  return <div className="h-2 bg-zinc-800 rounded overflow-hidden"><div className="h-full bg-emerald-500 transition-all" style={{ width: `${v * 100}%` }} /></div>
}

function AgentStatus() {
  const { data: a } = useAgent()
  const online = a?.seen && Date.now() - Date.parse(a.seen) < 20000
  const dot = (ok: boolean | undefined) => <span className={`inline-block w-2 h-2 rounded-full ${ok ? 'bg-emerald-500' : 'bg-rose-500'}`} />
  return (
    <div className="flex flex-wrap gap-4 text-sm text-zinc-400">
      <span>{dot(!!online)} агент {a?.host || '—'}</span>
      <span>{dot(a?.ollama)} Ollama</span>
      <span>{dot(a?.awake)} {a?.awake ? 'сон запрещён' : 'сон разрешён'}</span>
      {a?.models && <span>план: {a.models.planner} · работа: {a.models.worker} · проверка: {a.models.reviewer}</span>}
      {!!a?.tps && <span>{a.tps.toFixed(1)} ток/с</span>}
    </div>
  )
}

function NewTask({ onCreated }: { onCreated: (id: string) => void }) {
  const [goal, setGoal] = useState('')
  const submit = async () => {
    if (!goal.trim()) return
    const t = await api.createTask(goal.trim())
    setGoal('')
    qc.invalidateQueries({ queryKey: ['tasks'] })
    onCreated(t.id)
  }
  return (
    <div className="p-3 border-b border-zinc-800 space-y-2">
      <textarea value={goal} onChange={(e) => setGoal(e.target.value)} rows={3} placeholder="Новая главная задача…"
        className="w-full bg-zinc-900 border border-zinc-800 rounded p-2 text-sm outline-none focus:border-sky-600"
        onKeyDown={(e) => e.key === 'Enter' && e.metaKey && submit()} />
      <button onClick={submit} className="w-full bg-sky-600 hover:bg-sky-500 rounded py-1.5 text-sm">Поставить в очередь (⌘↵)</button>
    </div>
  )
}

export default function App() {
  useLive()
  const { data: tasks = [] } = useTasks()
  const [sel, setSel] = useState(location.hash.slice(1))
  useEffect(() => { location.hash = sel }, [sel])
  useEffect(() => { if (!sel && tasks.length) setSel(tasks[0].id) }, [tasks, sel])

  return (
    <div className="h-screen flex flex-col">
      <header className="px-4 py-2 border-b border-zinc-800 flex items-center gap-6">
        <h1 className="font-semibold">Оркестратор</h1>
        <AgentStatus />
      </header>
      <div className="flex-1 flex min-h-0">
        <aside className="w-72 border-r border-zinc-800 flex flex-col">
          <NewTask onCreated={setSel} />
          <div className="overflow-auto flex-1">
            {tasks.map((t) => (
              <button key={t.id} onClick={() => setSel(t.id)}
                className={`w-full text-left p-3 border-b border-zinc-900 space-y-1.5 hover:bg-zinc-900 ${sel === t.id ? 'bg-zinc-900' : ''}`}>
                <div className="text-sm line-clamp-2">{t.goal}</div>
                <div className="flex items-center gap-2"><Badge s={t.status} /><span className="text-xs text-zinc-500">{Math.round(t.progress * 100)}%</span></div>
                <Bar v={t.progress} />
              </button>
            ))}
          </div>
        </aside>
        <main className="flex-1 min-w-0">{sel ? <TaskView id={sel} onDeleted={() => setSel('')} /> : <div className="p-8 text-zinc-500">Создайте задачу слева</div>}</main>
      </div>
    </div>
  )
}

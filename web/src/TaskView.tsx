import { useEffect, useMemo, useRef, useState } from 'react'
import Markdown from 'react-markdown'
import * as echarts from 'echarts'
import {
  createColumnHelper, flexRender, getCoreRowModel, getFilteredRowModel, getSortedRowModel, useReactTable, type SortingState,
} from '@tanstack/react-table'
import { DndContext, closestCenter, type DragEndEvent } from '@dnd-kit/core'
import { SortableContext, arrayMove, useSortable, verticalListSortingStrategy } from '@dnd-kit/sortable'
import { CSS } from '@dnd-kit/utilities'
import { api, fmtDur, lastScore, qc, stepTime, statusRu, useEvents, useTask, type Step, type Task, fmtCost } from './api'
import { Badge, Bar } from './App'

const btn = 'px-3 py-1 rounded text-sm bg-zinc-800 hover:bg-zinc-700'

export default function TaskView({ id, onDeleted }: { id: string; onDeleted: () => void }) {
  const { data: t } = useTask(id)
  const [tab, setTab] = useState('table')
  const [step, setStep] = useState('')
  if (!t) return <div className="p-8 text-zinc-500">Загрузка…</div>
  const cmd = (kind: string, extra = {}) => api.command(t.id, kind, extra)
  const sel = t.steps.find((s) => s.id === step)
  const done = t.steps.filter((s) => s.status === 'done').length

  return (
    <div className="h-full flex">
      <div className="flex-1 min-w-0 flex flex-col">
        <div className="p-4 border-b border-zinc-800 space-y-3">
          <div className="flex items-start gap-3">
            <h2 className="flex-1 text-lg whitespace-pre-wrap">{t.goal}</h2>
            <Badge s={t.status} />
          </div>
          <div className="flex items-center gap-4">
            <div className="flex-1"><Bar v={t.progress} /></div>
            <span className="text-sm text-zinc-400">{Math.round(t.progress * 100)}% · шагов {done}/{t.steps.length}{t.score ? ` · итог ${t.score}/10` : ''}</span>
          </div>
          {t.summary && <p className="text-sm text-zinc-400">{t.summary}</p>}
          <div className="flex gap-2 flex-wrap">
            {t.status === 'paused' ? <button className={btn} onClick={() => cmd('resume')}>▶ Продолжить</button>
              : <button className={btn} onClick={() => cmd('pause')}>⏸ Пауза</button>}
            <button className={btn} onClick={() => cmd('cancel')}>✕ Отменить</button>
            <button className={btn + ' ml-auto text-rose-400'} onClick={async () => {
              if (!confirm('Удалить задачу?')) return
              await api.deleteTask(t.id); qc.invalidateQueries({ queryKey: ['tasks'] }); onDeleted()
            }}>Удалить</button>
          </div>
        </div>
        <div className="flex gap-1 px-4 pt-2 border-b border-zinc-800">
          {[['table', 'Шаги'], ['order', 'Порядок'], ['stats', 'Аналитика'], ['events', 'События']].map(([k, l]) => (
            <button key={k} onClick={() => setTab(k)} className={`px-3 py-1.5 text-sm rounded-t ${tab === k ? 'bg-zinc-800' : 'text-zinc-400'}`}>{l}</button>
          ))}
        </div>
        <div className="flex-1 overflow-auto p-4">
          {tab === 'table' && <StepsTable t={t} sel={step} onSel={setStep} />}
          {tab === 'order' && <Order t={t} onSel={setStep} />}
          {tab === 'stats' && <Stats t={t} />}
          {tab === 'events' && <Events id={t.id} t={t} />}
        </div>
      </div>
      {sel && <StepDetail t={t} s={sel} onClose={() => setStep('')} />}
    </div>
  )
}

const col = createColumnHelper<Step & { n: number }>()
const columns = [
  col.accessor('n', { header: '#', size: 40 }),
  col.accessor('title', { header: 'Шаг' }),
  col.accessor('status', { header: 'Статус', cell: (c) => <Badge s={c.getValue()} /> }),
  col.accessor('weight', { header: 'Вес' }),
  col.accessor((s) => s.attempts.length, { id: 'attempts', header: 'Попытки' }),
  col.accessor((s) => lastScore(s) ?? -1, { id: 'score', header: 'Оценка', cell: (c) => c.getValue() < 0 ? '—' : `${c.getValue()}/10` }),
  col.accessor((s) => stepTime(s), { id: 'time', header: 'Время', cell: (c) => c.getValue() ? fmtDur(c.getValue()) : '—' }),
  col.accessor((s) => s.attempts.reduce((a, x) => a + x.tokens, 0), { id: 'tokens', header: 'Токены' }),
  col.accessor((s) => s.attempts.reduce((a, x) => a + (x.cost ?? 0), 0), { id: 'cost', header: 'Цена', cell: (c) => fmtCost(c.getValue()) }),
]

function StepsTable({ t, sel, onSel }: { t: Task; sel: string; onSel: (id: string) => void }) {
  const [sorting, setSorting] = useState<SortingState>([])
  const [filter, setFilter] = useState('')
  const [status, setStatus] = useState('')
  const data = useMemo(() => t.steps.map((s, i) => ({ ...s, n: i + 1 })).filter((s) => !status || s.status === status), [t, status])
  const table = useReactTable({
    data, columns, state: { sorting, globalFilter: filter }, onSortingChange: setSorting, onGlobalFilterChange: setFilter,
    getCoreRowModel: getCoreRowModel(), getSortedRowModel: getSortedRowModel(), getFilteredRowModel: getFilteredRowModel(),
  })
  return (
    <div className="space-y-3">
      <div className="flex gap-2">
        <input value={filter} onChange={(e) => setFilter(e.target.value)} placeholder="Поиск…"
          className="bg-zinc-900 border border-zinc-800 rounded px-2 py-1 text-sm outline-none" />
        <select value={status} onChange={(e) => setStatus(e.target.value)} className="bg-zinc-900 border border-zinc-800 rounded px-2 py-1 text-sm">
          <option value="">все статусы</option>
          {['pending', 'running', 'done', 'failed', 'skipped'].map((s) => <option key={s} value={s}>{statusRu[s]}</option>)}
        </select>
      </div>
      <table className="w-full text-sm">
        <thead>
          {table.getHeaderGroups().map((hg) => (
            <tr key={hg.id} className="text-left text-zinc-500 border-b border-zinc-800">
              {hg.headers.map((h) => (
                <th key={h.id} onClick={h.column.getToggleSortingHandler()} className="py-2 pr-3 cursor-pointer select-none font-normal">
                  {flexRender(h.column.columnDef.header, h.getContext())}{{ asc: ' ↑', desc: ' ↓' }[h.column.getIsSorted() as string] ?? ''}
                </th>
              ))}
            </tr>
          ))}
        </thead>
        <tbody>
          {table.getRowModel().rows.map((r) => (
            <tr key={r.id} onClick={() => onSel(r.original.id)}
              className={`border-b border-zinc-900 cursor-pointer hover:bg-zinc-900 ${sel === r.original.id ? 'bg-zinc-900' : ''}`}>
              {r.getVisibleCells().map((c) => <td key={c.id} className="py-2 pr-3">{flexRender(c.column.columnDef.cell, c.getContext())}</td>)}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function SortItem({ s, n, onSel }: { s: Step; n: number; onSel: (id: string) => void }) {
  const locked = s.status !== 'pending'
  const { attributes, listeners, setNodeRef, transform, transition } = useSortable({ id: s.id, disabled: locked })
  return (
    <div ref={setNodeRef} style={{ transform: CSS.Transform.toString(transform), transition }}
      className="flex items-center gap-3 p-3 bg-zinc-900 border border-zinc-800 rounded">
      <span {...attributes} {...listeners} className={locked ? 'text-zinc-700' : 'cursor-grab text-zinc-400'}>⠿</span>
      <span className="text-zinc-500 w-6">{n}</span>
      <button onClick={() => onSel(s.id)} className="flex-1 text-left text-sm">{s.title}</button>
      <Badge s={s.status} />
    </div>
  )
}

function Order({ t, onSel }: { t: Task; onSel: (id: string) => void }) {
  const [items, setItems] = useState(t.steps)
  useEffect(() => setItems(t.steps), [t])
  const end = (e: DragEndEvent) => {
    if (!e.over || e.active.id === e.over.id) return
    const next = arrayMove(items, items.findIndex((s) => s.id === e.active.id), items.findIndex((s) => s.id === e.over!.id))
    setItems(next)
    api.command(t.id, 'reorder', { order: next.map((s) => s.id) })
  }
  return (
    <div className="space-y-2 max-w-3xl">
      <p className="text-sm text-zinc-500">Перетаскивайте ожидающие шаги, чтобы изменить порядок выполнения.</p>
      <DndContext collisionDetection={closestCenter} onDragEnd={end}>
        <SortableContext items={items.map((s) => s.id)} strategy={verticalListSortingStrategy}>
          {items.map((s, i) => <SortItem key={s.id} s={s} n={i + 1} onSel={onSel} />)}
        </SortableContext>
      </DndContext>
    </div>
  )
}

function Chart({ option }: { option: echarts.EChartsOption }) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const c = echarts.init(ref.current!, 'dark')
    c.setOption({ backgroundColor: 'transparent', tooltip: { trigger: 'axis' }, grid: { left: 40, right: 40, top: 30, bottom: 30 }, ...option })
    const r = () => c.resize()
    addEventListener('resize', r)
    return () => { removeEventListener('resize', r); c.dispose() }
  }, [option])
  return <div ref={ref} className="h-72 w-full" />
}

function Stats({ t }: { t: Task }) {
  const names = t.steps.map((_, i) => `${i + 1}`)
  const scores = useMemo<echarts.EChartsOption>(() => ({
    title: { text: 'Оценка и попытки по шагам', textStyle: { fontSize: 13 } },
    legend: { right: 0 }, xAxis: { type: 'category', data: names }, yAxis: [{ type: 'value', max: 10 }, { type: 'value' }],
    series: [
      { name: 'оценка', type: 'bar', data: t.steps.map((s) => lastScore(s) ?? 0) },
      { name: 'попытки', type: 'line', yAxisIndex: 1, data: t.steps.map((s) => s.attempts.length) },
    ],
  }), [t])
  const time = useMemo<echarts.EChartsOption>(() => ({
    title: { text: 'Время (мин) и скорость (ток/с)', textStyle: { fontSize: 13 } },
    legend: { right: 0 }, xAxis: { type: 'category', data: names }, yAxis: [{ type: 'value' }, { type: 'value' }],
    series: [
      { name: 'минуты', type: 'bar', data: t.steps.map((s) => +(stepTime(s) / 60).toFixed(1)) },
      { name: 'ток/с', type: 'line', yAxisIndex: 1, data: t.steps.map((s) => +(s.attempts.at(-1)?.tps ?? 0).toFixed(1)) },
    ],
  }), [t])
  const total = t.steps.reduce((a, s) => a + stepTime(s), 0)
  const tokens = t.steps.reduce((a, s) => a + s.attempts.reduce((b, x) => b + x.tokens, 0), 0)
  const card = (l: string, v: string) => <div className="bg-zinc-900 rounded p-3"><div className="text-xs text-zinc-500">{l}</div><div className="text-xl">{v}</div></div>
  return (
    <div className="space-y-6">
      <div className="grid grid-cols-2 md:grid-cols-3 gap-3">
        {card('Прогресс', `${Math.round(t.progress * 100)}%`)}
        {card('Время работы', fmtDur(total))}
        {card('Токенов', t.tokens_out ? `${(t.tokens_in ?? 0).toLocaleString()} → ${t.tokens_out.toLocaleString()}` : tokens.toLocaleString())}
        {card('Стоимость', fmtCost(t.cost ?? 0))}
        {card('Профиль', t.profile || '—')}
        {card('Раунд доработки', `${t.round}`)}
      </div>
      <Chart option={scores} />
      <Chart option={time} />
    </div>
  )
}

function Events({ id, t }: { id: string; t: Task }) {
  const { data = [] } = useEvents(id)
  const [kind, setKind] = useState('')
  const kinds = [...new Set(data.map((e) => e.kind))]
  const title = (sid?: string) => t.steps.find((s) => s.id === sid)?.title
  return (
    <div className="space-y-2">
      <select value={kind} onChange={(e) => setKind(e.target.value)} className="bg-zinc-900 border border-zinc-800 rounded px-2 py-1 text-sm">
        <option value="">все события</option>
        {kinds.map((k) => <option key={k}>{k}</option>)}
      </select>
      <div className="font-mono text-xs space-y-1">
        {data.filter((e) => !kind || e.kind === kind).map((e) => (
          <div key={e.id} className={`flex gap-3 ${e.kind === 'error' || e.kind === 'failed' ? 'text-rose-400' : ''}`}>
            <span className="text-zinc-600 shrink-0">{new Date(e.ts).toLocaleTimeString()}</span>
            <span className="text-sky-400 w-16 shrink-0">{e.kind}</span>
            <span className="whitespace-pre-wrap">{e.step_id && <span className="text-zinc-500">[{title(e.step_id)}] </span>}{e.msg}</span>
          </div>
        ))}
      </div>
    </div>
  )
}

function StepDetail({ t, s, onClose }: { t: Task; s: Step; onClose: () => void }) {
  const [n, setN] = useState(s.attempts.length)
  useEffect(() => setN(s.attempts.length), [s.id, s.attempts.length])
  const a = s.attempts[n - 1]
  return (
    <aside className="w-[45%] border-l border-zinc-800 flex flex-col">
      <div className="p-4 border-b border-zinc-800 space-y-2">
        <div className="flex items-start gap-2">
          <h3 className="flex-1 font-medium">{s.title}</h3>
          <button onClick={onClose} className="text-zinc-500">✕</button>
        </div>
        <Badge s={s.status} />
        <p className="text-sm text-zinc-400"><b className="text-zinc-300">Критерий:</b> {s.criteria}</p>
        <div className="flex gap-2">
          <button className={btn} onClick={() => api.command(t.id, 'retry', { step_id: s.id })}>↻ Повторить</button>
          <button className={btn} onClick={() => api.command(t.id, 'skip', { step_id: s.id })}>⤼ Пропустить</button>
        </div>
        {s.attempts.length > 0 && (
          <div className="flex gap-1 pt-1">
            {s.attempts.map((x) => (
              <button key={x.n} onClick={() => setN(x.n)}
                className={`px-2 py-0.5 rounded text-xs ${n === x.n ? 'bg-sky-700' : 'bg-zinc-800'} ${x.review?.passed ? 'ring-1 ring-emerald-500' : ''}`}>
                #{x.n} · {x.review?.score ?? '?'}/10
              </button>
            ))}
          </div>
        )}
      </div>
      {a ? (
        <div className="flex-1 overflow-auto p-4 space-y-4">
          {a.review && (
            <div className={`p-3 rounded text-sm ${a.review.passed ? 'bg-emerald-950' : 'bg-rose-950'}`}>
              <b>Проверка {a.review.score}/10:</b> {a.review.feedback}
            </div>
          )}
          <div className="text-xs text-zinc-500">{(a.tokens_in ?? 0).toLocaleString()} → {a.tokens} токенов · {fmtCost(a.cost ?? 0)} · {a.tps.toFixed(1)} ток/с · {fmtDur((Date.parse(a.finished) - Date.parse(a.started)) / 1000)}</div>
          <div className="prose prose-invert prose-sm max-w-none text-sm leading-relaxed [&_pre]:bg-zinc-900 [&_pre]:p-3 [&_pre]:rounded [&_pre]:overflow-auto [&_h1]:text-lg [&_h2]:text-base [&_ul]:list-disc [&_ul]:pl-5 [&_ol]:list-decimal [&_ol]:pl-5 [&_p]:my-2">
            <Markdown>{a.output}</Markdown>
          </div>
        </div>
      ) : <div className="p-4 text-sm text-zinc-500">Попыток пока нет</div>}
    </aside>
  )
}

import { QueryClient, useQuery } from '@tanstack/react-query'
import { useEffect } from 'react'

export type Review = { passed: boolean; score: number; feedback: string }
export type Attempt = { n: number; output: string; review?: Review; started: string; finished: string; tokens: number; tokens_in: number; cost: number; tps: number }
export type Step = { id: string; title: string; criteria: string; weight: number; status: string; attempts: Attempt[] }
export type Task = {
  id: string; goal: string; status: string; progress: number; score: number; summary: string
  round: number; steps: Step[]; created: string; updated: string
  profile: string; models?: Record<string, string>; tokens_in: number; tokens_out: number; cost: number
}
export type Event = { id: number; task_id: string; step_id?: string; ts: string; kind: string; msg: string }
export type Agent = { host: string; awake: boolean; ollama: boolean; models: Record<string, string>; profiles?: string[]; task_id: string; tps: number; seen: string }

export const qc = new QueryClient()

async function req<T>(method: string, url: string, body?: unknown): Promise<T> {
  const r = await fetch(url, { method, body: body ? JSON.stringify(body) : undefined, headers: { 'Content-Type': 'application/json' } })
  if (!r.ok) throw new Error(await r.text())
  const t = await r.text()
  return t ? JSON.parse(t) : (undefined as T)
}

export const api = {
  createTask: (goal: string, profile: string) => req<Task>('POST', '/api/tasks', { goal, profile }),
  deleteTask: (id: string) => req('DELETE', `/api/tasks/${id}`),
  command: (id: string, kind: string, extra: { step_id?: string; order?: string[] } = {}) =>
    req('POST', `/api/tasks/${id}/commands`, { kind, ...extra }),
}

export const useTasks = () => useQuery({ queryKey: ['tasks'], queryFn: () => req<Task[]>('GET', '/api/tasks') })
export const useTask = (id: string) => useQuery({ queryKey: ['task', id], queryFn: () => req<Task>('GET', `/api/tasks/${id}`), enabled: !!id })
export const useEvents = (id: string) => useQuery({ queryKey: ['events', id], queryFn: () => req<Event[]>('GET', `/api/tasks/${id}/events`), enabled: !!id })
export const useAgent = () => useQuery({ queryKey: ['agent'], queryFn: () => req<Agent>('GET', '/api/agent'), refetchInterval: 10000 })

export function useLive() {
  useEffect(() => {
    const es = new EventSource('/api/stream')
    es.onmessage = (m) => {
      const { kind, task_id } = JSON.parse(m.data)
      if (kind === 'agent') qc.invalidateQueries({ queryKey: ['agent'] })
      if (kind === 'task') { qc.invalidateQueries({ queryKey: ['tasks'] }); qc.invalidateQueries({ queryKey: ['task', task_id] }) }
      if (kind === 'event') qc.invalidateQueries({ queryKey: ['events', task_id] })
    }
    return () => es.close()
  }, [])
}

export const statusColor: Record<string, string> = {
  queued: 'bg-zinc-700', planning: 'bg-violet-600', running: 'bg-sky-600', paused: 'bg-amber-600',
  done: 'bg-emerald-600', failed: 'bg-rose-600', canceled: 'bg-zinc-600', pending: 'bg-zinc-700', skipped: 'bg-zinc-600',
}
export const statusRu: Record<string, string> = {
  queued: 'в очереди', planning: 'планирование', running: 'в работе', paused: 'пауза', done: 'готово',
  failed: 'ошибка', canceled: 'отменена', pending: 'ожидает', skipped: 'пропущен',
}

export const lastScore = (s: Step) => s.attempts.at(-1)?.review?.score
export const stepTime = (s: Step) => s.attempts.reduce((a, x) => a + (Date.parse(x.finished) - Date.parse(x.started)) / 1000, 0)
export const fmtDur = (sec: number) => sec < 60 ? `${Math.round(sec)}с` : sec < 3600 ? `${Math.round(sec / 60)}м` : `${(sec / 3600).toFixed(1)}ч`
export const fmtCost = (c: number) => !c ? '$0' : c < 0.01 ? '$' + c.toFixed(4) : '$' + c.toFixed(2)

import React, { useEffect, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { createBrowserRouter, RouterProvider, Link, Outlet } from 'react-router-dom'
import { Toaster, toast } from 'sonner'
import { invoke } from '@tauri-apps/api/core'

function useBackendUrl() {
  const [url, setUrl] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    async function load() {
      try {
        const backend = await invoke<string>('get_backend_url')
        // Retry health check up to ~15s
        for (let i = 0; i < 50 && !cancelled; i++) {
          const ok = await fetch(backend + '/health').then(r => r.ok).catch(() => false)
          if (ok) {
            if (!cancelled) setUrl(backend)
            return
          }
          await new Promise(r => setTimeout(r, 300))
        }
        if (!cancelled) setError('Backend not ready')
      } catch (e) {
        if (!cancelled) setError('Failed to get backend URL')
      }
    }
    load()
    return () => { cancelled = true }
  }, [])

  return { url, error }
}

function Home() {
  return (
    <div className="container">
      <h1>Personal CRM (Desktop)</h1>
      <p><Link to="/dashboard">Go to Dashboard</Link></p>
      <p><Link to="/contacts">Go to Contacts</Link></p>
      <p><Link to="/reminders">Go to Reminders</Link></p>
      <p><Link to="/settings">Go to Settings</Link></p>
    </div>
  )
}

type OverdueContact = {
  id: string
  full_name: string
  email?: string
  phone?: string
  cadence?: string
  last_contacted?: string
  days_overdue: number
  suggested_action: string
}

type Contact = {
  id: string
  full_name: string
  email?: string
  phone?: string
  location?: string
  cadence?: string
  birthday?: string
  last_contacted?: string
}

type DueReminder = {
  id: string
  title: string
  description?: string
  due_date: string
  completed: boolean
  contact_id?: string
  contact_name?: string
  contact_email?: string
}

function Reminders() {
  const { url, error } = useBackendUrl()
  const [items, setItems] = React.useState<DueReminder[] | null>(null)
  const [loading, setLoading] = React.useState(false)
  const [completingIds, setCompletingIds] = React.useState<Set<string>>(new Set())

  const load = React.useCallback(async () => {
    if (!url) return
    setLoading(true)
    try {
      const r = await fetch(url + '/api/v1/reminders')
      const j = await r.json()
      setItems(j?.data || [])
    } catch { toast.error('Failed to load reminders') }
    finally { setLoading(false) }
  }, [url])

  React.useEffect(() => { if (url) load() }, [url, load])

  const complete = async (id: string) => {
    if (!url) return
    try {
      setCompletingIds(prev => { const s = new Set(prev); s.add(id); return s })
      const res = await fetch(url + `/api/v1/reminders/${id}/complete`, { method: 'PATCH' })
      if (!res.ok) throw new Error()
      toast.success('Reminder completed')
      load()
    } catch { toast.error('Failed to complete reminder') }
    finally { setCompletingIds(prev => { const s = new Set(prev); s.delete(id); return s }) }
  }

  if (error) return (<div style={{padding:20}}>Error: {error}</div>)
  if (!url) return (<div style={{padding:20}}>Loading backend...</div>)

  return (
    <div className="container">
      <h2>Reminders</h2>
      {loading && <div className="spinner" />}
      {!loading && items && items.length === 0 && (<div>No reminders</div>)}
      {!loading && items && items.length > 0 && (
        <div style={{display:'grid', gap:8}}>
          {items.map(r => (
            <div key={r.id} style={{display:'flex', justifyContent:'space-between', alignItems:'center'}} className="card">
              <div>
                <div style={{fontWeight:600}}>{r.title}</div>
                <div style={{fontSize:12, color:'#555'}}>Due: {new Date(r.due_date).toLocaleDateString()} {r.completed ? '• Completed' : ''}</div>
                {r.contact_name && (<div style={{fontSize:12, color:'#555'}}>{r.contact_name}</div>)}
              </div>
              <div>
                {!r.completed && (
                  <button onClick={()=>complete(r.id)} className="btn" disabled={completingIds.has(r.id)}>Complete</button>
                )}
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

function Settings() {
  const { url, error } = useBackendUrl()
  const [busy, setBusy] = React.useState(false)
  const [file, setFile] = React.useState<File | null>(null)

  const exportData = async () => {
    if (!url) return
    setBusy(true)
    try {
      const res = await fetch(url + '/api/v1/export', { method: 'POST' })
      if (!res.ok) throw new Error()
      const blob = await res.blob()
      const a = document.createElement('a')
      a.href = URL.createObjectURL(blob)
      a.download = `personal-crm-backup-${new Date().toISOString().slice(0,10)}.json`
      a.click()
      URL.revokeObjectURL(a.href)
      toast.success('Backup downloaded')
    } catch { toast.error('Export failed') }
    finally { setBusy(false) }
  }

  const importData = async () => {
    if (!url || !file) return
    setBusy(true)
    try {
      const fd = new FormData()
      fd.append('backup', file)
      const res = await fetch(url + '/api/v1/import', { method: 'POST', body: fd })
      if (!res.ok) throw new Error()
      const j = await res.json()
      const meta = j?.data?.metadata || {}
      toast.success(`Validated: ${meta.contacts_count||0} contacts, ${meta.reminders_count||0} reminders`)
    } catch { toast.error('Import failed') }
    finally { setBusy(false) }
  }

  if (error) return (<div style={{padding:20}}>Error: {error}</div>)
  if (!url) return (<div style={{padding:20}}>Loading backend...</div>)

  return (
    <div className="container">
      <h2>Settings</h2>
      <div style={{display:'flex', gap:8, margin:'12px 0'}}>
        <button disabled={busy} onClick={exportData} className="btn">Download Backup</button>
        <input type="file" accept=".json" onChange={e=> setFile(e.target.files?.[0]||null)} className="btn" />
        <button disabled={busy || !file} onClick={importData} className="btn">Validate Import</button>
      </div>
    </div>
  )
}

function Contacts() {
  const { url, error } = useBackendUrl()
  const [data, setData] = React.useState<{contacts: Contact[]; total: number; page: number; pages: number} | null>(null)
  const [loading, setLoading] = React.useState(false)
  const [search, setSearch] = React.useState('')
  const [sort, setSort] = React.useState<'name'|'location'|'birthday'|'last_contacted'|'cadence'|''>('')
  const [order, setOrder] = React.useState<'asc'|'desc'>('asc')
  const [busyIds, setBusyIds] = React.useState<Set<string>>(new Set())

  const fetchList = React.useCallback(async () => {
    if (!url) return
    setLoading(true)
    const params = new URLSearchParams()
    params.set('page','1')
    params.set('limit','1000')
    if (search) params.set('search', search)
    // Backend supports name, location, birthday, last_contacted; cadence is client-side only
    if (sort && sort !== 'cadence') params.set('sort', sort)
    if (order) params.set('order', order)
    try {
      const r = await fetch(url + '/api/v1/contacts?' + params.toString())
      const j = await r.json()
      let list: Contact[] = j?.data || []
      // Apply client-side sort for cadence
      if (sort === 'cadence') {
        list = list.slice().sort((a, b) => {
          const av = (a.cadence || '').toLowerCase()
          const bv = (b.cadence || '').toLowerCase()
          if (av === bv) return 0
          return av < bv ? -1 : 1
        })
        if (order === 'desc') list.reverse()
      }
      const meta = j?.meta?.pagination || { page:1, pages:1, total:list.length }
      setData({ contacts: list, total: meta.total, page: meta.page, pages: meta.pages })
    } catch {
      toast.error('Failed to load contacts')
    } finally {
      setLoading(false)
    }
  }, [url, search, sort, order])

  React.useEffect(() => {
    if (!url) return
    fetchList()
  }, [url, fetchList])

  const markContacted = async (id: string) => {
    if (!url) return
    try {
      setBusyIds(prev => { const s = new Set(prev); s.add(id); return s })
      const res = await fetch(url + `/api/v1/contacts/${id}/last-contacted`, { method: 'PATCH' })
      if (!res.ok) throw new Error()
      toast.success('Marked as contacted')
      fetchList()
    } catch { toast.error('Failed to mark contacted') }
    finally { setBusyIds(prev => { const s = new Set(prev); s.delete(id); return s }) }
  }

  if (error) return (<div style={{padding:20}}>Error: {error}</div>)
  if (!url) return (<div style={{padding:20}}>Loading backend...</div>)

  return (
    <div className="container">
      <h2>Contacts</h2>
      <div style={{display:'flex', gap:8, margin:'12px 0'}}>
        <input value={search} onChange={e=>setSearch(e.target.value)} placeholder="Search" className="btn" style={{padding:6}} />
        <button onClick={fetchList} className="btn">Search</button>
        <select value={sort} onChange={e=>setSort(e.target.value as any)} className="btn">
          <option value="">No sort</option>
          <option value="name">Name</option>
          <option value="location">Location</option>
          <option value="birthday">Birthday</option>
          <option value="last_contacted">Last Contacted</option>
          <option value="cadence">Cadence</option>
        </select>
        <select value={order} onChange={e=>setOrder(e.target.value as any)} className="btn">
          <option value="asc">Asc</option>
          <option value="desc">Desc</option>
        </select>
      </div>
      {loading && (<div className="spinner" />)}
      {!loading && data && (
        <div>
          <div style={{fontSize:12, color:'#555', marginBottom:8}}>{data.total} contacts</div>
          <div style={{display:'grid', gap:8}}>
            {data.contacts.map(c => (
              <div key={c.id} className="card">
                <div style={{display:'flex', justifyContent:'space-between', alignItems:'center'}}>
                  <div>
                    <div style={{fontWeight:600}}>{c.full_name}</div>
                    <div style={{fontSize:12, color:'#555'}}>
                      {c.email || ''}{c.email && c.phone ? ' • ' : ''}{c.phone || ''}
                    </div>
                    <div style={{fontSize:12, color:'#555', marginTop:6}}>
                      {c.location ? `Location: ${c.location}` : 'Location: -'} • {c.cadence ? `Cadence: ${c.cadence}` : 'Cadence: -'}
                    </div>
                    <div style={{fontSize:12, color:'#555', marginTop:4}}>
                      Birthday: {c.birthday ? new Date(c.birthday).toLocaleDateString() : '-'} • Last Contacted: {c.last_contacted ? new Date(c.last_contacted).toLocaleDateString() : 'Never'}
                    </div>
                  </div>
                  <div>
                    <button onClick={()=>markContacted(c.id)} className="btn" disabled={busyIds.has(c.id)}>Mark Contacted</button>
                  </div>
                </div>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}

function Dashboard() {
  const { url, error } = useBackendUrl()
  const [items, setItems] = React.useState<OverdueContact[] | null>(null)
  const [loading, setLoading] = React.useState(false)
  const [busyIds, setBusyIds] = React.useState<Set<string>>(new Set())

  React.useEffect(() => {
    if (!url) return
    let cancelled = false
    setLoading(true)
    fetch(url + '/api/v1/contacts/overdue')
      .then(r => r.json())
      .then((res) => {
        // API envelope: { data, error, meta }
        const data = res?.data || []
        if (!cancelled) setItems(data)
      })
      .catch(() => {
        toast.error('Failed to load overdue contacts')
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => { cancelled = true }
  }, [url])

  const markContacted = async (id: string) => {
    if (!url) return
    try {
      setBusyIds(prev => { const s = new Set(prev); s.add(id); return s })
      const res = await fetch(url + `/api/v1/contacts/${id}/last-contacted`, { method: 'PATCH' })
      if (!res.ok) throw new Error()
      toast.success('Marked as contacted')
      // Refresh list
      setItems(null)
      const r = await fetch(url + '/api/v1/contacts/overdue')
      const j = await r.json()
      setItems(j?.data || [])
    } catch {
      toast.error('Failed to mark contacted')
    } finally { setBusyIds(prev => { const s = new Set(prev); s.delete(id); return s }) }
  }

  if (error) return (<div style={{padding:20}}>Error: {error}</div>)
  if (!url) return (<div style={{padding:20}}>Loading backend...</div>)

  return (
    <div className="container">
      <h2>Action Required</h2>
      <div style={{marginBottom:12, color:'#555'}}>Backend: {url}</div>
      {loading && (<div className="spinner" />)}
      {!loading && items && items.length === 0 && (
        <div>All caught up! 🎉</div>
      )}
      {!loading && items && items.length > 0 && (
        <div style={{display:'grid', gap:12}}>
          {items.map(c => (
            <div key={c.id} className="card">
              <div style={{display:'flex', justifyContent:'space-between', alignItems:'center'}}>
                <div>
                  <div style={{fontWeight:600}}>{c.full_name}</div>
                  <div style={{fontSize:12,color:'#555'}}>{c.days_overdue} days overdue • {c.cadence || 'no cadence'}</div>
                  <div style={{fontSize:12,marginTop:6,color:'#1d4ed8'}}>💡 {c.suggested_action}</div>
                </div>
                <div>
                  <button onClick={() => markContacted(c.id)} className="btn" disabled={busyIds.has(c.id)}>Mark as Contacted</button>
                </div>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

function Layout() {
  return (
    <div>
      <div className="nav">
        <div className="container nav-inner">
          <div style={{fontWeight:600}}>Personal CRM</div>
          <div>
            <Link to="/dashboard">Dashboard</Link>
            <Link to="/contacts">Contacts</Link>
            <Link to="/reminders">Reminders</Link>
            <Link to="/settings">Settings</Link>
          </div>
        </div>
      </div>
      <Outlet />
    </div>
  )
}

const router = createBrowserRouter([
  {
    path: '/', element: <Layout />, children: [
      { index: true, element: <Dashboard /> },
      { path: 'dashboard', element: <Dashboard /> },
      { path: 'contacts', element: <Contacts /> },
      { path: 'reminders', element: <Reminders /> },
      { path: 'settings', element: <Settings /> },
    ]
  }
])

function App() {
  return (<><Toaster position="top-right" richColors /><RouterProvider router={router} /></>)
}

const el = document.getElementById('root')!
createRoot(el).render(<App />)

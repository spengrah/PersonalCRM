import React from 'react'
import { createRoot } from 'react-dom/client'

function App() {
  return (<div style={{padding:20}}>Desktop UI Skeleton</div>)
}

const el = document.getElementById('root')!
createRoot(el).render(<App />)

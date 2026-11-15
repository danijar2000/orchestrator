import { createRoot } from 'react-dom/client'
import { QueryClientProvider } from '@tanstack/react-query'
import { qc } from './api'
import App from './App'
import './index.css'

createRoot(document.getElementById('root')!).render(
  <QueryClientProvider client={qc}>
    <App />
  </QueryClientProvider>,
)

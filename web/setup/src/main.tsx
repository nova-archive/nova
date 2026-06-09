import React from 'react'
import ReactDOM from 'react-dom/client'

// Minimal first-run wizard shell (M13 Task 9 scaffold). Task 10 replaces this
// placeholder with the real stepper (master key → answers → commit). Kept
// dependency-light and hermetic: no CDN fonts/scripts.
function App() {
  return (
    <main>
      <h1>Nova Setup</h1>
      <p>First-run setup wizard.</p>
    </main>
  )
}

ReactDOM.createRoot(document.getElementById('root') as HTMLElement).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
)

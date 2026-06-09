import React from 'react'
import ReactDOM from 'react-dom/client'

// Self-hosted IBM Plex (latin), bundled by Vite — no Google Fonts / CDN. The
// hermetic-spa gate fails the build on any external origin in dist.
import '@fontsource/ibm-plex-sans/latin-400.css'
import '@fontsource/ibm-plex-sans/latin-500.css'
import '@fontsource/ibm-plex-sans/latin-600.css'
import '@fontsource/ibm-plex-serif/latin-400.css'
import '@fontsource/ibm-plex-serif/latin-500.css'
import '@fontsource/ibm-plex-mono/latin-400.css'
import '@fontsource/ibm-plex-mono/latin-500.css'

import './styles/tokens.css'
import './styles/base.css'

import { Wizard } from './wizard/Wizard'

ReactDOM.createRoot(document.getElementById('root') as HTMLElement).render(
  <React.StrictMode>
    <Wizard />
  </React.StrictMode>,
)

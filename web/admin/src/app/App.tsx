import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom'

import { AuthProvider } from '../auth/AuthProvider'
import { ToastProvider } from '../ui/toast'
import { RequireAuth } from './RequireAuth'
import { Shell } from './Shell'

import { LoginScreen } from '../screens/Login'
import { CallbackScreen } from '../screens/Callback'
import { BlobsScreen } from '../screens/Blobs'
import { BlobDetailScreen } from '../screens/BlobDetail'
import { ModerationScreen } from '../screens/Moderation'
import { AuditsScreen } from '../screens/Audits'
import { KeysScreen } from '../screens/Keys'
import { JobsScreen } from '../screens/Jobs'
import { AuditLogScreen } from '../screens/AuditLog'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: { retry: 1, refetchOnWindowFocus: false, staleTime: 5_000 },
  },
})

export function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <BrowserRouter basename="/admin">
        <AuthProvider>
          <ToastProvider>
            <Routes>
              <Route path="/login" element={<LoginScreen />} />
              <Route path="/callback" element={<CallbackScreen />} />
              <Route
                element={
                  <RequireAuth>
                    <Shell />
                  </RequireAuth>
                }
              >
                <Route index element={<Navigate to="blobs" replace />} />
                <Route path="blobs" element={<BlobsScreen />} />
                <Route path="blobs/:cid" element={<BlobDetailScreen />} />
                <Route path="moderation" element={<ModerationScreen />} />
                <Route path="audits" element={<AuditsScreen />} />
                <Route path="keys" element={<KeysScreen />} />
                <Route path="jobs" element={<JobsScreen />} />
                <Route path="audit-log" element={<AuditLogScreen />} />
              </Route>
              <Route path="*" element={<Navigate to="/blobs" replace />} />
            </Routes>
          </ToastProvider>
        </AuthProvider>
      </BrowserRouter>
    </QueryClientProvider>
  )
}

'use client';

import { useState } from 'react';
import { api } from '@/lib/api';
import type { Connection } from '@/lib/types';
import { Banner, Card, Pill } from './ui';

type Props = { connection: Connection; onChanged: () => void };

export function DrivePanel({ connection, onChanged }: Props) {
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<{ tone: 'ok' | 'error'; text: string } | null>(null);

  async function run(action: () => Promise<void>, success: string) {
    setBusy(true);
    setNote(null);
    try {
      await action();
      setNote({ tone: 'ok', text: success });
      onChanged();
    } catch (e) {
      const err = e as { message?: string };
      setNote({ tone: 'error', text: err.message ?? 'The request failed.' });
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card title="Google Drive connection">
      {note ? (
        <Banner tone={note.tone === 'ok' ? 'ok' : 'error'}>{note.text}</Banner>
      ) : null}

      {connection.connected ? (
        <>
          <div className="row">
            <Pill tone="on">Connected</Pill>
            <span className="muted">
              {connection.uses_app_data_folder
                ? 'Hidden storage (appDataFolder) — not visible in Drive web/app'
                : connection.folder_present
                  ? 'Storvia/ folder present'
                  : 'Storvia/ folder missing'}
              {connection.subject ? ` · ${connection.subject}` : ''}
              {connection.granted_scopes ? ` · ${connection.granted_scopes}` : ''}
            </span>
            <span style={{ flex: 1 }} />
            {!connection.uses_app_data_folder && (
              <button
                className="ghost"
                disabled={busy}
                onClick={() =>
                  run(() => api.ensureFolder().then(() => undefined), 'The Storvia/ folder is present in Drive.')
                }
              >
                Create Storvia/ folder
              </button>
            )}
            <button
              className="danger"
              disabled={busy}
              onClick={() => {
                if (!window.confirm('Disconnect Google Drive? Your Drive files are left alone.')) return;
                void run(() => api.disconnect().then(() => undefined), 'Google Drive disconnected.');
              }}
            >
              Disconnect
            </button>
          </div>
        </>
      ) : (
        <>
          <div className="row">
            <Pill tone="bad">{connection.status || 'Not connected'}</Pill>
            <span className="muted">Uploads will use the fallback provider until Drive is connected.</span>
          </div>
          <div className="row" style={{ marginTop: 16 }}>
            <a href="/console/sign-in">
              <button>Connect Google Drive</button>
            </a>
          </div>
        </>
      )}
    </Card>
  );
}

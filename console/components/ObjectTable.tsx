'use client';

import { useMemo, useState } from 'react';
import { api, downloadUrl, formatBytes } from '@/lib/api';
import type { ObjectView } from '@/lib/types';
import { Banner, Card, Pill } from './ui';

type Tone = 'on' | 'off' | 'warn' | 'bad' | 'drive';

/**
 * What each stored-object state is called in front of a person.
 *
 * The API returns the raw enum (`ready`, `uploading`, `missing`, ...), which is the right
 * thing to send and the wrong thing to show. Each state gets a plain-language name and a
 * tone, so the table reads as a status rather than as a database column.
 */
const STATUS: Record<string, { label: string; tone: Tone }> = {
  ready: { label: 'Stored', tone: 'on' },
  pending: { label: 'Waiting', tone: 'warn' },
  uploading: { label: 'Uploading', tone: 'warn' },
  deleting: { label: 'Deleting', tone: 'warn' },
  failed: { label: 'Failed', tone: 'bad' },
  missing: { label: 'Missing', tone: 'bad' },
  deleted: { label: 'Deleted', tone: 'off' },
};

function statusOf(status: string): { label: string; tone: Tone } {
  return STATUS[status] ?? { label: status, tone: 'off' };
}

/** The provider a file lives on, in words rather than as an enum. */
function providerLabel(provider: string): string {
  if (provider === 'google_drive') return 'Google Drive';
  if (provider === 's3_compatible') return 'Fallback storage';
  return provider || 'Unknown';
}

/**
 * A provider reference is only worth showing once the file actually reached a provider.
 *
 * Before that the recorded value is the *planned* identity, which for Drive happens to be
 * the object's own id. Showing it would look like a real Drive file id and send anyone
 * checking it on a hunt for a file that does not exist yet.
 */
function referenceOf(object: ObjectView): string {
  if (object.status !== 'ready' && object.status !== 'missing') return '';
  return object.provider_ref ?? '';
}

function messageOf(error: unknown): string {
  const err = error as { message?: string };
  return err?.message ?? 'The delete failed.';
}

export function ObjectTable({
  objects,
  onChanged,
  usesAppDataFolder,
}: {
  objects: ObjectView[];
  /** Re-reads the console state. Reloading the page instead would lose the scroll
   *  position and any open panel, for a change that only touches this table. */
  onChanged?: () => void | Promise<void>;
  /** True when files are stored in Drive's hidden appDataFolder. Files there are
   *  invisible to the user in Drive web and mobile. */
  usesAppDataFolder?: boolean;
}) {
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [clearing, setClearing] = useState(false);

  const counts = useMemo(() => {
    let stored = 0;
    let working = 0;
    let problem = 0;
    for (const object of objects) {
      if (object.status === 'ready') stored += 1;
      else if (object.status === 'pending' || object.status === 'uploading' || object.status === 'deleting')
        working += 1;
      else problem += 1;
    }
    return { stored, working, problem };
  }, [objects]);

  async function refresh() {
    if (onChanged) await onChanged();
    else window.location.reload();
  }

  async function remove(object: ObjectView) {
    const where = providerLabel(object.provider);
    if (!window.confirm(`Delete “${object.name}” from ${where}?\n\nThis removes the file at the provider and cannot be undone.`)) {
      return;
    }
    setBusyId(object.id);
    setError(null);
    setNotice(null);
    try {
      await api.deleteObject(object.id);
      setNotice(`Deleted “${object.name}”.`);
      await refresh();
    } catch (e) {
      setError(messageOf(e));
    } finally {
      setBusyId(null);
    }
  }

  async function removeAll() {
    if (
      !window.confirm(
        `Delete all ${objects.length} stored objects from their providers?\n\nThis cannot be undone.`,
      )
    ) {
      return;
    }
    setClearing(true);
    setError(null);
    setNotice(null);
    try {
      const result = await api.deleteAllObjects();
      const failed = result.failed ?? [];
      if (failed.length === 0) {
        setNotice(`Deleted ${result.deleted} ${result.deleted === 1 ? 'object' : 'objects'}.`);
      } else {
        // Report both halves. "Deleted everything" would be false, and staying silent
        // about the failures would leave files in Drive the operator believes are gone.
        const detail = failed
          .map((failure) => `${failure.Name || failure.ObjectID} — ${failure.Reason}`)
          .join('; ');
        setError(`Deleted ${result.deleted}, but ${failed.length} could not be removed: ${detail}`);
      }
      await refresh();
    } catch (e) {
      setError(messageOf(e));
    } finally {
      setClearing(false);
    }
  }

  /** Drive web link for a stored file. provider_ref is the Drive file id. */
  function driveFileUrl(object: ObjectView): string {
    if (!object.provider_ref) return '';
    return `https://drive.google.com/file/d/${object.provider_ref}/view`;
  }

  const summary = [
    `${counts.stored} stored`,
    counts.working > 0 ? `${counts.working} in progress` : null,
    counts.problem > 0 ? `${counts.problem} need attention` : null,
  ]
    .filter(Boolean)
    .join(' · ');

  return (
    <Card
      title={
        objects.length > 0 ? (
          <>
            Stored objects <span className="obj-count">({summary})</span>
          </>
        ) : (
          'Stored objects'
        )
      }
      hint={
        usesAppDataFolder
          ? 'Files are stored in Drive’s hidden appDataFolder — not visible in Drive web or mobile. Storvia can still read and delete them.'
          : 'Files are stored in a “Storvia” folder in your Drive. Each row is one file; deleting removes it from the provider that holds it, not just from this list.'
      }
      action={
        objects.length > 0 ? (
          <button className="danger btn-sm" disabled={clearing} onClick={() => void removeAll()}>
            {clearing ? 'Deleting…' : 'Delete all'}
          </button>
        ) : null
      }
    >
      {error ? (
        <Banner tone="error" title="Some files were not deleted.">
          {error}
        </Banner>
      ) : null}
      {notice ? <Banner tone="ok">{notice}</Banner> : null}

      {objects.length === 0 ? (
        <p className="muted">
          Nothing stored yet. Upload a file above and it will appear here with where it landed.
        </p>
      ) : (
        <table>
          <thead>
            <tr>
              <th scope="col">File</th>
              <th scope="col">Stored in</th>
              <th scope="col">Status</th>
              <th scope="col">Added</th>
              <th scope="col" aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {objects.map((object) => {
              const status = statusOf(object.status);
              const reference = referenceOf(object);
              const busy = busyId === object.id || clearing;
              const driveUrl = driveFileUrl(object);
              return (
                <tr key={object.id} title={`Object id: ${object.id}`}>
                  <td>
                    <div className="obj-name">{object.name}</div>
                    <div className="obj-detail">
                      {object.category || object.mime_type || 'file'}
                      {object.size ? ` · ${formatBytes(object.size)}` : ''}
                    </div>
                  </td>
                  <td>
                    <Pill tone={object.provider === 'google_drive' ? 'drive' : 'off'}>
                      {providerLabel(object.provider)}
                    </Pill>
                    {reference ? (
                      <div className="obj-ref" title={reference}>
                        {reference}
                      </div>
                    ) : (
                      <div className="obj-detail">not at a provider yet</div>
                    )}
                  </td>
                  <td>
                    <Pill tone={status.tone}>{status.label}</Pill>
                  </td>
                  <td className="muted small">{new Date(object.created_at).toLocaleString()}</td>
                  <td className="actions">
                    {object.downloadable ? (
                      <a className="mono" href={downloadUrl(object.id)}>
                        Download
                      </a>
                    ) : null}
                    {driveUrl ? (
                      <a className="mono" href={driveUrl} target="_blank" rel="noopener noreferrer">
                        View in Drive
                      </a>
                    ) : null}
                    <button
                      className="danger btn-sm"
                      disabled={busy}
                      onClick={() => void remove(object)}
                    >
                      {busyId === object.id ? 'Deleting…' : 'Delete'}
                    </button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </Card>
  );
}

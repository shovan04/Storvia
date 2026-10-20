/** The shapes the console API returns. Kept in one place so a Go change that renames a
 *  field is a type error here rather than an `undefined` at runtime. */

export type RoutingInfo = {
  forced: boolean;
  configured: boolean;
  overridden: boolean;
  target_provider: string;
  available: boolean;
};

export type StorageInfo = {
  drive_connected: boolean;
  fallback_enabled: boolean;
  fallback_vendor?: string;
  fallback_ready: boolean;
  fallback_requires_consent: boolean;
  accepts_uploads: boolean;
  blocked_reason?: string;
  stores_outside_drive: boolean;
};

export type Connection = {
  connected: boolean;
  status: string;
  subject?: string;
  granted_scopes?: string;
  folder_present: boolean;
  stale: boolean;
  last_success_at?: string;
  /** True when files are stored in Drive's hidden appDataFolder instead of a
   *  visible Storvia/ folder. Files there are invisible to the user in Drive
   *  web and mobile. */
  uses_app_data_folder: boolean;
};

export type ObjectView = {
  id: string;
  name: string;
  logical_key: string;
  mime_type: string;
  category: string;
  size: number;
  provider: 'google_drive' | 's3_compatible';
  status: string;
  provider_ref?: string;
  created_at: string;
  downloadable: boolean;
};

export type State = {
  signed_in: boolean;
  account: {
    tenant_id: string;
    user_id: string;
    /** The connected Google account. Display only -- subject is the identity. */
    display_name?: string;
    email?: string;
    picture_url?: string;
    subject?: string;
    /** What the navbar shows when there is no display name. */
    label?: string;
  };
  connection: Connection;
  storage: StorageInfo;
  consent: { granted: boolean; granted_at?: string };
  routing: RoutingInfo;
  faults: { enabled: boolean };
  objects: ObjectView[];
  diagnostics?: Record<string, unknown> | null;
};

/** One object a bulk delete could not remove, and why. */
export type BulkDeleteFailure = {
  ObjectID: string;
  Name: string;
  Reason: string;
};

/** The result of clearing every stored object. Reports both outcomes, because a bulk
 *  delete can partly succeed and claiming otherwise would mislead the operator. */
export type DeleteAllResult = {
  deleted: number;
  /** Null when nothing failed: Go marshals a nil slice as null, not an empty array. */
  failed?: BulkDeleteFailure[] | null;
};

export type UploadResult = {
  provider: string;
  provider_ref?: string;
  object_id: string;
  upload_id: string;
  name: string;
  size: number;
  mime_type?: string;
  outcome: string;
  elapsed: string;
  detail?: string;
  bytes_sent: number;
};

/** The routing control is tri-state: forcing, preferring Drive, or deferring to config.
 *  A plain boolean cannot express the third option, which is the one operators reach for
 *  most often. */
export type RoutingChoice = 'forced' | 'drive' | 'config';

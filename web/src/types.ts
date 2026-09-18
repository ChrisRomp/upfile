export interface Collection<T> { items: T[]; total: number }
export interface Settings {
  configured: boolean;
  max_file_bytes: number;
  storage_budget_bytes: number;
  default_link_hours: number;
  stored_bytes: number;
  reserved_bytes: number;
  chunk_bytes: number;
  lease_seconds: number;
  max_records: number;
  record_count: number;
  cleanup_errors: number | string[];
}
export interface Container {
  id: string;
  name: string;
  instructions: string;
  max_file_bytes: number | null;
  effective_max_bytes?: number;
  created_at: number;
  last_activity?: number;
  status: string;
  file_count: number;
  stored_bytes: number;
  active_uploads: number;
  active_downloads: number;
  link_count: number;
}
export interface Link {
  id: string;
  container_id: string;
  sender_label: string;
  expires_at: number;
  max_file_bytes: number | null;
  effective_max_bytes: number;
  status: string;
  created_at: number;
  file_count: number;
  active_uploads: number;
}
export interface CreatedContainer extends Container {
  initial_link: Link & { url: string };
}
export interface ReceivedFile {
  id: string;
  container_id: string;
  link_id: string;
  name: string;
  original_name: string;
  sender_label: string;
  comment: string;
  size: number;
  created_at: number;
  status: string;
}
export interface Audit {
  id: string;
  actor: string;
  action: string;
  target: string;
  created_at: number;
}
export interface PublicLink {
  id: string;
  title: string;
  instructions: string;
  max_file_bytes: number;
  expires_at: number;
  chunk_bytes: number;
  session_expires_at: number;
  busy: boolean;
  reset_available: boolean;
}
export interface Attempt {
  id: string;
  status: 'uploading' | 'finalizing' | 'completed' | 'canceled' | 'abandoned';
  size: number;
  offset: number;
  upload_url: string;
  created_at: number;
}

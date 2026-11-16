<!-- 中文说明：本地优先工作台，以逻辑对话为主视图，并保留底层资产证据和人工确认入口。 -->
<script lang="ts">
  import { onMount, tick } from 'svelte';
  import { Activity, ArrowLeft, ArrowRight, Check, ChevronRight, Copy, Database, ExternalLink, FolderOpen, KeyRound, Inbox, ListFilter, LoaderCircle, MessagesSquare, PanelLeft, RefreshCw, Search, ShieldCheck, TriangleAlert, X } from '@lucide/svelte';
  import ThemeSwitcher from './ThemeSwitcher.svelte';
  import NavigationIcon from './NavigationIcon.svelte';
  import SettingsPage from './settings/SettingsPage.svelte';
  import SettingsPanel from './settings/SettingsPanel.svelte';
  import GitHubConnection from './settings/GitHubConnection.svelte';
  import DensityControl from './settings/DensityControl.svelte';
  import CodexDiscovery, { type CodexHomeCandidate } from './settings/CodexDiscovery.svelte';
  import AnalyticsPage from './analytics/AnalyticsPage.svelte';
  import MaintenancePage from './maintenance/MaintenancePage.svelte';
  import type { MaintenanceReport } from './maintenance/types';
  import type { AnalyticsReport, AnalyticsSelection } from './analytics/types';
  import type { SettingsCategory } from './settings/catalog';
  import { parseRoute, routePath, type AppRoute, type RoutePage } from './router';
  import LibraryDrawer from './sessions/LibraryDrawer.svelte';
  import ArchivesPage from './archives/ArchivesPage.svelte';
  import ArchiveImpact from './archives/ArchiveImpact.svelte';
  import type { ArchivePreview } from './archives/types';
  import ConversationReader from './sessions/ConversationReader.svelte';
  import { readerLocationFromURL, type ReaderEvent, type ReaderLocation } from './sessions/reader-state';
  import OperationChoices from './sessions/OperationChoices.svelte';
  import OperationWorkflow from './sessions/OperationWorkflow.svelte';
  import OperationImpact from './sessions/OperationImpact.svelte';
  import { impactBlockerMessage, type RestrictedImpact } from './sessions/impact';
  import DestinationConfirmation from './sessions/DestinationConfirmation.svelte';
  import ExecutionConfirmationDialog from './sessions/ExecutionConfirmation.svelte';
  import ExecutionProgress from './sessions/ExecutionProgress.svelte';
  import { executionResultLabel, type ExecutionConfirmation, type ExecutionTask } from './sessions/execution';
  import { archiveLocationMissing, destinationsReady, type OperationDestinations } from './sessions/destinations';
  import { emptySessionFilters, parseSessionView, sessionViewSearch, type FilterSnapshot } from './sessions/view-state';
  import { completedRemovalIDs, readConversationSelection, writeConversationSelection } from './sessions/selection';
  import { projectFamilies, reviewFamilyItems } from './sessions/families';

  type AssetStatus = 'stable' | 'active_writing' | 'growing' | 'pending_jsonl_line' | 'open_by_process' | 'writer_lock_observed' | 'stale_lock' | 'sqlite_wal_present' | 'source_changed_during_probe' | 'blocked_for_mutation' | 'unknown' | 'conflict' | 'unsupported';
  type CompactPane = 'workspace' | 'sessions' | 'detail';
  type WorkspaceView = RoutePage;
  type DetailView = 'reading' | 'technical';
  type Density = 'comfortable' | 'compact' | 'reading';
  type TokenUsage = {
    input_tokens: number;
    cached_input_tokens: number;
    output_tokens: number;
    reasoning_output_tokens: number;
    total_tokens: number;
  };
  type Diagnostic = {
    code: string;
    severity: string;
    message: string;
    line?: number;
    evidence?: string;
    read_only: boolean;
  };
  type FileInventory = {
    relative_path: string;
    kind: string;
    size_bytes: number;
    mod_time: string;
    status: AssetStatus;
    error?: string;
    changed_during_scan: boolean;
    jsonl?: {
      line_count: number;
      invalid_json_lines: number;
      invalid_utf8_lines: number;
      pending_lines: number;
      max_line_bytes: number;
      top_level_types: Record<string, number>;
    };
    rollout?: {
      conversation_id: string;
      diagnostics?: Diagnostic[];
    };
  };
  type ConversationSummary = {
    id: string;
    thread_kind?: string;
    automation_kind?: string;
    parent_thread_id?: string;
    parent_conversation_id?: string;
    child_conversation_ids?: string[];
    descendant_count?: number;
    parent_missing?: boolean;
    family_conflict?: boolean;
    agent_path?: string;
    agent_nickname?: string;
    forked_from_id?: string;
    source_id?: string;
    title?: string;
    first_user_text?: string;
    cwd?: string;
    model?: string;
    model_provider?: string;
    cli_version?: string;
    started_at?: string;
    updated_at?: string;
    rollout_paths: string[];
    event_count: number;
    user_messages: number;
    assistant_messages: number;
    turn_count: number;
    token_usage: TokenUsage;
    status: AssetStatus;
    diagnostics?: Diagnostic[];
    orphan: boolean;
    asset_graph?: ConversationAssetGraph;
    asset_set_hash?: string;
    related_asset_paths?: string[];
  };
  type SavedFilter = {
    id: string;
    name: string;
    filters: FilterSnapshot;
  };
  type InventoryReport = {
    root: string;
    read_only: boolean;
    mode?: string;
    source_read_only?: boolean;
    generated_at: string;
    snapshot_status?: 'missing' | 'fresh' | 'stale';
    stale?: boolean;
    scan_task_id?: string;
    last_successful_scan_at?: string;
    refresh_requested?: boolean;
    freshness_check_error?: string;
    files: FileInventory[];
    conversations: ConversationSummary[];
    file_total?: number;
    conversation_total?: number;
    errors?: string[];
    analytics?: AnalyticsReport;
    maintenance?: MaintenanceReport;
    adapter?: {
      identity?: { version?: string; schema_fingerprint?: string; schema_fingerprints?: string[] };
      descriptor?: { id: string; read_only_reason?: string; capabilities?: Record<string, boolean> };
      matched: boolean;
      reason: string;
    };
  };
  type WorkspaceDefinition = {
    id: string;
    name: string;
    root: string;
    archive_db?: string;
    created_at: string;
    updated_at: string;
  };
  type WorkspaceResponse = {
    persisted: boolean;
    registry_path?: string;
    current_id?: string;
    current?: WorkspaceDefinition;
    workspaces: WorkspaceDefinition[];
  };
  type SearchHit = {
    conversation_id?: string;
    relative_path: string;
    line: number;
    start: number;
    end: number;
    snippet: string;
    top_type: string;
    source_status?: AssetStatus;
    activity_status?: AssetStatus;
    live_consistency?: string;
    read_only: boolean;
  };
  type ActivityEvidence = {
    probed_at: string;
    interval_ms: number;
    window_index?: number;
    window_count?: number;
    before_size_bytes: number;
    after_size_bytes: number;
    before_mod_time?: string;
    after_mod_time?: string;
    changed_during_probe: boolean;
    source_changed_during_probe?: boolean;
    source_identity_before?: string;
    source_identity_after?: string;
    source_hash_before?: string;
    source_hash_after?: string;
    source_hash_algorithm?: string;
    source_hash_compared?: boolean;
    jsonl_pending_line?: boolean;
    sqlite_wal_present?: boolean;
    sqlite_shm_present?: boolean;
    sqlite_auxiliary?: string[];
    open_by_process?: Array<{ pid: number; command?: string }>;
    matching_lock_ids?: string[];
    lock_age_seconds?: number;
    sqlite_busy?: boolean;
    process_probe_available: boolean;
    process_probe_reason?: string;
    lock_probe_available: boolean;
    lock_probe_reason?: string;
    sqlite_probe_reason?: string;
    observed_states?: AssetStatus[];
    mutation_blocked: boolean;
    readable: boolean;
    lifecycle_blocked: boolean;
    history?: Array<{ probed_at: string; status: AssetStatus; observed_states?: AssetStatus[]; changed: boolean; reason?: string }>;
    reason?: string;
  };
  type ActivityResult = {
    relative_path: string;
    kind: string;
    status: AssetStatus;
    activity_status?: AssetStatus;
    mutation_blocked: boolean;
    evidence: ActivityEvidence;
    error?: string;
  };
  type ActivityResponse = {
    read_only: boolean;
    root: string;
    conversation_id?: string;
    snapshot_status?: string;
    snapshot_stale?: boolean;
    snapshot_generated_at?: string;
    readable: boolean;
    lifecycle_blocked: boolean;
    activity: {
      generated_at: string;
      interval_ms: number;
      window_count: number;
      windows?: Array<{ index: number; probed_at: string; stable_count: number; active_count: number; blocked_count: number; changed_count: number }>;
      process_probe_available: boolean;
      process_probe_reason?: string;
      lock_probe_available: boolean;
      lock_probe_reason?: string;
      lock_file_count: number;
      results: ActivityResult[];
    };
  };
  type ConversationEvent = {
    id: string;
    role?: string;
    kind: string;
    top_level_type: string;
    payload_type?: string;
    text: string;
    truncated: boolean;
    line: number;
    start_offset: number;
    end_offset: number;
    timestamp?: string;
  };
  type AssetGraphNode = {
    id: string;
    kind: string;
    label?: string;
    relative_path?: string;
    asset_kind?: string;
    thread_id?: string;
    table?: string;
    column?: string;
    row_key?: string;
    row_key_kind?: string;
    line?: number;
    status: AssetStatus;
    size_bytes?: number;
    mod_time?: string;
    source?: string;
  };
  type AssetGraphEdge = {
    from: string;
    to: string;
    relation: string;
    evidence: string;
    confidence: string;
  };
  type ConversationAssetGraph = {
    version: number;
    conversation_id: string;
    completeness: string;
    nodes: AssetGraphNode[];
    edges: AssetGraphEdge[];
    diagnostics?: Diagnostic[];
    hash: string;
  };
  type HomeTrendPoint = AnalyticsReport['daily'][number] & { label: string };
  type ConversationDetail = {
    conversation: ConversationSummary;
    files: FileInventory[];
  };
  type AssetGraphPageResponse = {
    asset_graph?: ConversationAssetGraph;
    node_total: number;
    node_offset: number;
    node_limit: number;
    node_has_more: boolean;
    edge_total: number;
    edge_offset: number;
    edge_limit: number;
    edge_has_more: boolean;
    has_more: boolean;
    error?: string;
  };
  type ProjectFileSummary = {
    relative_path: string;
    kind: string;
    size_bytes: number;
    mod_time: string;
  };
  type ProjectRelationReport = {
    conversation_id: string;
    requested_path?: string;
    project_root?: string;
    git_root?: string;
    git_branch?: string;
    git_remote?: string;
    working_directory_relative?: string;
    status: string;
    confidence: string;
    file_count: number;
    ignored_file_count: number;
    symlink_count: number;
    total_bytes: number;
    truncated: boolean;
    top_level_dirs?: string[];
    key_files?: ProjectFileSummary[];
    recent_files?: ProjectFileSummary[];
    evidence?: Array<{ kind: string; value: string; confidence: string }>;
    limitations?: string[];
    read_only: boolean;
    generated_at: string;
  };
  type SummaryEvidence = { kind: string; label: string };
  type ConversationSummaryResult = {
    conversation_id: string;
    provider_id: string;
    text: string;
    generated_at: string;
    confidence: string;
    evidence?: SummaryEvidence[];
    limitations?: string[];
    redacted: boolean;
    redaction_count?: number;
  };
  type SummaryTask = {
    id: string;
    kind: string;
    root: string;
    provider_id: string;
    status: string;
    created_at: string;
    started_at?: string;
    completed_at?: string;
    requested: number;
    matched: number;
    completed: number;
    failed: number;
    current_conversation_id?: string;
    results?: ConversationSummaryResult[];
    errors?: Array<{ conversation_id: string; error: string }>;
    error?: string;
  };
  type UpdatePlan = {
    id: string;
    version: string;
    previous_version?: string;
    file_count: number;
    total_bytes: number;
    allow_downgrade: boolean;
    created_at: string;
  };
  type UpdateStatus = {
    install_root: string;
    state: {
      active_version?: string;
      previous_version?: string;
      last_status?: string;
      last_error?: string;
      updated_at?: string;
    };
    versions: string[];
    pending_plan_ids?: string[];
    recovery_required: boolean;
    recovery_message?: string;
    read_only_source: boolean;
    signature_required: boolean;
    trusted_key_count: number;
    online_configured: boolean;
    online_feed_url?: string;
  };
  type OnlineRelease = {
    version: string;
    build_id?: string;
    os: string;
    arch: string;
    url: string;
    sha256: string;
    size_bytes: number;
    notes?: string;
  };
  type OnlineCheck = {
    feed_url: string;
    current_version?: string;
    available: boolean;
    release?: OnlineRelease;
    checked_at: string;
  };
  type UpdatePreflight = {
    status: string;
    blocked: boolean;
    migration_available: boolean;
    migration: { status?: string; current_version?: number; supported_version?: number; future_version?: boolean; requires_migration?: boolean };
    adapter: { valid: boolean; errors?: string[] };
  };
  type RecoveryItem = {
    operation_id: string;
    conversation_id?: string;
    source_id?: string;
    phase: string;
    created_at: string;
    updated_at: string;
    asset_count: number;
    bytes: number;
    receipt_count: number;
    error_code?: string;
    error_message?: string;
    recovery_note?: string;
    has_control_plane_snapshot: boolean;
    disposition: string;
    can_retry: boolean;
    can_rollback: boolean;
    external_state_unknown: boolean;
    reason?: string;
  };
  type RecoveryResponse = {
    read_only: boolean;
    available: boolean;
    items: RecoveryItem[];
    terminal_items?: RecoveryItem[];
    errors?: string[];
  };
  type LocalRestorePlan = {
    restore_operation_id: string;
    source_journal_path?: string;
    source_operation_id?: string;
    archive_path: string;
    target_root: string;
    control_plane_bound: boolean;
    preview: {
      encrypted: boolean;
      ready: boolean;
      files: Array<{ relative_path: string; target_path: string; size_bytes: number; sha256: string }>;
      conflicts?: string[];
      control_plane_snapshot: boolean;
    };
  };
  type LocalArchiveItem = {
    path: string;
    name: string;
    operation_id: string;
    conversation_id?: string;
    created_at: string;
    encrypted: boolean;
    asset_count: number;
    control_plane_snapshot: boolean;
    declared_payload_bytes: number;
    staging: boolean;
  };
  type LocalRestoreRecoveryItem = {
    restore_operation_id: string;
    source_operation_id?: string;
    archive_path: string;
    target_root: string;
    phase: string;
    updated_at: string;
    asset_count: number;
    error_code?: string;
    error_message?: string;
    recovery_note?: string;
    has_control_plane_snapshot: boolean;
  };
  type LocalRestoreRecoveryResponse = { available: boolean; items: LocalRestoreRecoveryItem[]; errors?: string[] };
  type CopyTask = {
    id: string;
    kind: string;
    status: string;
    phase?: string;
    source_root?: string;
    destination_root?: string;
    files_done?: number;
    files_total?: number;
    bytes_done?: number;
    bytes_total?: number;
    plan?: {
      source_root: string;
      destination_root: string;
      file_count: number;
      total_bytes: number;
      source_manifest_sha256: string;
      marker?: { kind?: string; adapter_id?: string; schema_sha256?: string; sqlite_semantics_sha256?: string };
      excluded_paths?: string[];
      warnings?: string[];
      activity?: {
        process_probe_available?: boolean;
        process_probe_reason?: string;
        lock_probe_available?: boolean;
        lock_probe_reason?: string;
        lock_file_count?: number;
        lock_ids?: string[];
        blocked_count?: number;
        results?: Array<{ relative_path: string; status: string; activity_status?: string; evidence?: { reason?: string; observed_states?: string[]; mutation_blocked?: boolean } }>;
      };
    };
    result?: { published?: boolean; verified?: boolean; destination_root?: string; file_count?: number; total_bytes?: number; source_manifest_sha256?: string };
    error?: string;
  };
  type ImpactPreview = {
    items?: BatchOperationPreview['items'];
    preview_id?: string;
    source_root?: string;
    restricted_preview?: RestrictedImpact;
    archive_preview?: ArchivePreview;
    execution_scope?: string;
    blocked: boolean;
    error?: string;
    asset_graph?: ConversationAssetGraph;
    plan?: {
      operation_id: string;
      status: string;
      requires_confirmation?: boolean;
      assets: Array<{ relative_path: string; size_bytes: number; sha256: string }>;
      actions: string[];
      verification: string[];
    };
    activity?: {
      process_probe_available: boolean;
      process_probe_reason?: string;
      lock_probe_available: boolean;
      lock_probe_reason?: string;
      results: Array<{
        relative_path: string;
        status: AssetStatus;
        error?: string;
        evidence: {
          changed_during_probe: boolean;
          open_by_process?: Array<{ pid: number; command?: string }>;
          matching_lock_ids?: string[];
          sqlite_busy?: boolean;
          observed_states?: AssetStatus[];
          source_changed_during_probe?: boolean;
          source_hash_compared?: boolean;
          jsonl_pending_line?: boolean;
          sqlite_wal_present?: boolean;
          mutation_blocked?: boolean;
          readable?: boolean;
          lifecycle_blocked?: boolean;
          reason?: string;
        };
      }>;
    };
    adapter?: InventoryReport['adapter'];
  };
  type BatchOperationPreview = {
    preview_id?: string;
    restricted_preview?: RestrictedImpact;
    archive_preview?: ArchivePreview;
    execution_scope?: string;
    read_only: boolean;
    source_read_only: boolean;
    blocked: boolean;
    error?: string;
    batch_id: string;
    source_root?: string;
    summary: {
      requested_conversations: number;
      conversations: number;
      prepared: number;
      blocked: number;
      assets: number;
      bytes: number;
    };
    items: Array<{
      automatically_included?: boolean;
      parent_source_id?: string;
      thread_kind?: string;
      conversation_id: string;
      source_id?: string;
      title?: string;
      discovered_assets?: Array<{ relative_path: string; kind: string; size_bytes: number; status: AssetStatus; owners: number }>;
      blocked: boolean;
      error?: string;
      completed?: boolean;
      activity?: ImpactPreview['activity'];
      asset_graph?: ConversationAssetGraph;
      plan?: ImpactPreview['plan'];
      result?: { needs_review?: boolean; phase?: string; journal_path?: string };
    }>;
  };
  type OperationOptions = {
    archive: boolean;
    archive_local?: boolean;
    archive_remote?: boolean;
    encrypted: boolean;
    trash: boolean;
    permanent_delete: boolean;
  };
  type PendingSourceAsset = {
    relative_path: string;
    kind: string;
    size_bytes: number;
    mod_time: string;
    sha256?: string;
  };
  type PendingOperation = {
    operation_id: string;
    workspace: string;
    source_root: string;
    conversation_id: string;
    asset_set: PendingSourceAsset[];
    asset_set_hash: string;
    strategy: OperationOptions;
    created_at: string;
    expires_at?: string;
    state: string;
    block_reasons?: string[];
    last_probe_at?: string;
    last_reconfirmed_at?: string;
    probe_window_count?: number;
    required_window_count?: number;
    last_activity?: { window_count?: number; results?: Array<{ relative_path: string; status: string; evidence?: { reason?: string; open_by_process?: Array<{ pid: number; command?: string }>; matching_lock_ids?: string[] } }> };
    preview?: { operation_id?: string; status?: string; requires_confirmation?: boolean; actions?: string[]; assets?: Array<{ relative_path: string; size_bytes: number; sha256: string }>; verification?: string[]; shared_asset_paths?: string[] };
  };
  type PendingOperationResponse = {
    read_only: boolean;
    source_read_only: boolean;
    expired?: boolean;
    requires_review?: boolean;
    operation: PendingOperation;
    idempotent?: boolean;
    error?: string;
  };
  type GitHubAuthSession = {
    id: string;
    status: string;
    account: string;
    user_code: string;
    verification_uri: string;
    verification_uri_complete?: string;
    expires_at: string;
    created_at: string;
    updated_at: string;
    error?: string;
  };
  type GitHubAuthStatus = {
    configured: boolean;
    credential_available: boolean;
    device_flow_available?: boolean;
    cli_available?: boolean;
    session?: GitHubAuthSession;
  };
  type GitHubArchiveSummary = {
    archive_id: string;
    manifest_path: string;
    created_at?: string;
    file_count: number;
    chunk_count: number;
    source_manifest_sha256?: string;
  };
  type RemoteArchiveProviderInfo = {
    id: string;
    name: string;
    target_required: boolean;
    supports_listing: boolean;
    supports_recovery: boolean;
    supports_delete: boolean;
  };
  type GitHubRepositorySummary = {
    owner: string;
    name: string;
    full_name: string;
    description?: string;
    private: boolean;
    default_branch?: string;
    html_url?: string;
  };
  type GitHubDownloadPlan = {
    operation_id: string;
    archive_id: string;
    destination: string;
    output_directory: string;
    expires_at: string;
    archive: GitHubArchiveSummary;
  };
  type RemoteRestorePlan = {
    restore_operation_id: string;
    archive_id: string;
    archive_path: string;
    staging_destination?: string;
    expires_at: string;
    plan: LocalRestorePlan;
  };
  type GitHubDeletePlan = {
    operation_id: string;
    archive_id: string;
    expires_at: string;
    confirmation_required: string;
    archive: GitHubArchiveSummary;
  };

  type LocalAuthStatus = {
    enabled: boolean;
    bound: boolean;
    password_configured: boolean;
    active_session_count: number;
    binding_expires_at?: string;
  };
  type TrashDetection = {
    os: string;
    architecture: string;
    candidate_path?: string;
    default_path?: string;
    configured: boolean;
    exists: boolean;
    is_directory: boolean;
    writable: boolean;
    supported: boolean;
    read_only: boolean;
    reason: string;
  };
  type PlatformCapability = {
    id: string;
    name: string;
    status: 'available' | 'limited' | 'unavailable' | 'unverified' | string;
    read_only: boolean;
    reason: string;
  };
  type PlatformCapabilitiesResponse = {
    read_only: boolean;
    source_read_only: boolean;
    platform: {
      os: string;
      architecture: string;
      read_only: boolean;
      capabilities: PlatformCapability[];
    };
  };

  let report: InventoryReport | null = null;
  let recentConversations: ConversationSummary[] = [];
  let homeProjects: NonNullable<AnalyticsReport['projects']> = [];
  let homeTrendPoints: HomeTrendPoint[] = [];
  let homeTrendPeak = 1;
  let lifecycleWritesEnabled = false;
  let lifecycleFixtureOnly = false;
  let authConfigured = false;
  let authRequired = false;
  let authBound = false;
  let authBindingExpiresAt = '';
  let authDevice = '浏览器';
  let authCode = '';
  let authPassword = '';
  let authMessage = '';
  let authLoading = false;
  let sessionToken = '';
  let applicationStarted = false;
  let inventoryReady = false;
  let workspaces: WorkspaceDefinition[] = [];
  let currentWorkspace: WorkspaceDefinition | null = null;
  let workspaceLoading = false;
  let workspaceMessage = '';
  let workspaceName = '';
  let workspaceRoot = '';
  let workspaceArchiveDB = '';
  let workspacePersisted: boolean | null = null;
  let workspaceRegistryPath = '';
  let workspaceCandidates: CodexHomeCandidate[] = [];
  let workspaceDiscoveryLoading = false;
  let workspaceDiscoveryMessage = '';
  let workspaceServiceAddress = '';
  let trashDetection: TrashDetection | null = null;
  let trashDetectionLoading = false;
  let trashDetectionMessage = '';
  let trashCustomPath = '';
  let trashSettingsLoading = false;
  let trashSettingsMessage = '';
  let trashSettingsError = false;
  let executionOpen = false;
  let executionChallenge: ExecutionConfirmation | null = null;
  let executionOrigin: 'single' | 'batch' = 'single';
  let executionPreviewID = '';
  let executionTargets: Array<{ id: string; title: string; automaticallyIncluded?: boolean; subagent?: boolean }> = [];
  let expandedFamilyIDs = new Set<string>();
  let executionSourceRoot = '';
  let executionPackagedTrash = false;
  let executionStrategy: OperationOptions = { archive: false, encrypted: false, trash: true, permanent_delete: false };
  let executionPassword = '';
  let executionBusy = false;
  let executionMessage = '';
  let executionTasks: ExecutionTask[] = [];
  let executionTask: ExecutionTask | null = null;
  let executionResult: ExecutionTask | null = null;
  let submittedExecutionID = '';
  let executionPollTimer: ReturnType<typeof setTimeout> | undefined;
  let executionPolling = false;
  let executionTasksRequestToken = 0;
  let lifecycleRestricted = false;
  let lifecycleRealConversations = false;
  let reconciledExecutionID = '';
  let destinationOpen = false;
  let destinationBusy = false;
  let destinationMessage = '';
  let destinationStatus: OperationDestinations | null = null;
  let destinationNeedsArchive = false;
  let destinationNeedsTrash = true;
  let platformCapabilities: PlatformCapabilitiesResponse['platform'] | null = null;
  let platformCapabilitiesLoading = false;
  let platformCapabilitiesMessage = '';
  let activeWorkspace: WorkspaceView = 'home';
  let detailView: DetailView = 'reading';
  let summaryExpanded = false;
  let compactPane: CompactPane = 'sessions';
  let density: Density = 'comfortable';
  let densityStorageError = false;
  let settingsCategory: SettingsCategory = 'appearance';
  let routeConversationID = '';
  let routeConversationLoading = false;
  let routeConversationError = '';
  let routeNotFoundPath = '';
  let routeRestoreToken = 0;
  let copiedValue = '';
  let activeFilterLabels: Array<{ key: keyof FilterSnapshot | 'time' | 'tokens' | 'bytes'; label: string; value: string }> = [];
  let selectedConversation: ConversationSummary | null = null;
  let conversationActivity: ActivityResponse | null = null;
  let activityLoading = false;
  let activityMessage = '';
  let selectedAsset: FileInventory | null = null;
  let previewStart = 0;
  let previewLength = 0;
  let search = '';
  let searchDraft = '';
  let searchRequestToken = 0;
  let searchExecutedQuery = '';
  let statusFilter = '全部状态';
  let conversationQuery = '';
  let cwdFilter = '';
  let modelFilter = '';
  let modelExact = '';
  let cwdExact = '';
  let fromFilter = '';
  let toFilter = '';
  let minTokensFilter = '';
  let maxTokensFilter = '';
  let minBytesFilter = '';
  let maxBytesFilter = '';
  let diagnosticsFilter = '';
  let conversationFiltersOpen = false;
  let filterDraft = emptySessionFilters();
  let batchPanelOpen = false;
  let reviewPanelOpen = false;
  let projectSearch = '';
  let visibleProjectFilters: NonNullable<AnalyticsReport['projects']> = [];
  let savedFilterName = '';
  let savedFilterStore: Record<string, SavedFilter[]> = {};
  let savedFilters: SavedFilter[] = [];
  let loading = false;
  let errorMessage = '';
  let preview = '';
  let searchHits: SearchHit[] = [];
  let searchLimited = false;
  let searching = false;
  let impactPreview: ImpactPreview | null = null;
  let previewLoading = false;
  let impactPreviewRequestToken = 0;
  let batchOperationPreview: BatchOperationPreview | null = null;
  let batchOperationLoading = false;
  let batchPreviewRequestToken = 0;
  let batchOperationExecuting = false;
  let operationExecutable = false;
  let batchOperationExecutable = false;
  let batchOperationMessage = '';
  let batchSharedError = '';
  let batchRawPreviewOpen = false;
  let singleTechnicalOpen = false;
  let selectedConversationIDs = new Set<string>();
  let selectionRoot = '';
  let selectionStorageError = false;
  let selectionCopyMessage = '';
  let selectionCopyLoading = false;
  let latestBatchReview: { selection_origin: string; preview: BatchOperationPreview } | null = null;
  let operationOptions: OperationOptions = { archive: true, archive_local: true, archive_remote: true, encrypted: false, trash: true, permanent_delete: false };
  let singleImpactPanel: HTMLDivElement;
  let batchImpactPanel: HTMLDivElement;
  let operationConfirmationCode = '';
  let operationArchivePassword = '';
  let operationExecuting = false;
  let operationMessage = '';
  let pendingOperations: PendingOperation[] = [];
  let pendingLoading = false;
  let pendingBusy = '';
  let pendingMessage = '';
  let pendingLoadError = '';
  let pendingTab: 'conversations' | 'operations' | 'recovery' = 'conversations';
  let pendingSearchDraft = '';
  let pendingQuery = '';
  let pendingFilter = '';
  let pendingConversationItems: ConversationSummary[] = [];
  let pendingConversationTotal = 0;
  let pendingConversationLoading = false;
  let pendingConversationError = '';
  let pendingConversationStale = false;
  let pendingConversationRequestToken = 0;
  let pendingOperationsRequestToken = 0;
  let pendingSelectedID = '';
  let pendingDrawerOpen = false;
  let pendingConfirmationCode = '';
  let pendingArchivePassword = '';
  let pendingPreviewCode = '';
  let pendingReviewed = false;
  let events: ConversationEvent[] = [];
  let readerLocation: ReaderLocation | null = null;
  let eventTotal = 0;
  let eventsLoading = false;
  let eventsLoadingMore = false;
  let eventOffset = 0;
  let eventRequestToken = 0;
  let summaryResults: Record<string, ConversationSummaryResult> = {};
  let conversationSummary: ConversationSummaryResult | null = null;
  let summaryLoading = false;
  let summaryMessage = '';
  let summaryProviderID = 'local-deterministic';
  let summaryProviderIDs = ['local-deterministic'];
  let summaryTask: SummaryTask | null = null;
  let summaryBatchLoading = false;
  let summaryPollToken = 0;
  let updateStatus: UpdateStatus | null = null;
  let updatePreflight: UpdatePreflight | null = null;
  let onlineUpdate: OnlineCheck | null = null;
  let updatePlan: UpdatePlan | null = null;
  let updateBundleRoot = '';
  let updateLoading = false;
  let updateMessage = '';
  let projectRelation: ProjectRelationReport | null = null;
  let projectLoading = false;
  let projectMessage = '';
  let githubStatus: GitHubAuthStatus | null = null;
  let githubSession: GitHubAuthSession | null = null;
  let githubAccount: { login: string; name?: string } | null = null;
  let githubAccountLoading = false;
  let githubAccountMessage = '';
  let githubCodeMessage = '';
  let githubLoading = false;
  let githubMessage = '';
  let githubPollTimer: ReturnType<typeof setTimeout> | null = null;
  let githubStatusLoading = false;
  let githubArchives: GitHubArchiveSummary[] = [];
  let remoteProvider: RemoteArchiveProviderInfo | null = null;
  let remoteProviderConfigured = false;
  let remoteTargetRequired = false;
  let githubArchivesConfigured = false;
  let githubArchivesCredentialAvailable = false;
  let githubArchivesLoading = false;
  let githubArchiveMessage = '';
  let githubArchiveVerifying = '';
  let githubArchiveDownloading = '';
  let githubDownloadDestination = '';
  let githubDownloadPlan: GitHubDownloadPlan | null = null;
  let remoteRestoreTargetRoot = '';
  let remoteRestoreSourceOperationID = '';
  let remoteRestorePassword = '';
  let remoteRestorePlan: RemoteRestorePlan | null = null;
  let githubArchiveRestoring = '';
  let githubDeletePlan: GitHubDeletePlan | null = null;
  let githubDeleteConfirmation = '';
  let githubArchiveDeleting = '';
  let githubRepositories: GitHubRepositorySummary[] = [];
  let githubRepositoryQuery = '';
  let githubRepositoryLimit = 20;
  let githubArchiveLimit = 20;
  let githubArchiveTargetSaving = false;
  $: githubRepositoryMatches = githubRepositories.filter(repository => repository.full_name.toLocaleLowerCase().includes(githubRepositoryQuery.trim().toLocaleLowerCase()));
  let githubRepositoriesConfigured = false;
  let githubRepositoriesCredentialAvailable = false;
  let githubRepositoriesLoading = false;
  let githubRepositoryCreating = false;
  let githubRepositoryName = 'codex-chronicle-archive';
  let githubRepositoryDescription = '';
  let githubCreatedRepository: GitHubRepositorySummary | null = null;
  let githubRepositoryMessage = '';
  let githubArchiveTarget: GitHubRepositorySummary | null = null;
  let githubArchiveTargetLoading = false;
  let assetGraphLoading = false;
  let assetGraphLoadingMore = false;
  let assetGraphMessage = '';
  let assetGraphNodeOffset = 0;
  let assetGraphEdgeOffset = 0;
  let assetGraphNodeTotal = 0;
  let assetGraphEdgeTotal = 0;
  let assetGraphHasMore = false;
  let recoveryItems: RecoveryItem[] = [];
  let recoveryAvailable = false;
  let recoveryErrors: string[] = [];
  let recoveryLoading = false;
  let recoveryRequestToken = 0;
  let recoveryExecuting = '';
  let recoveryMessage = '';
  let localRestoreArchivePath = '';
  let localArchives: LocalArchiveItem[] = [];
  let localArchivesLoading = false;
  let localArchivesAvailable = false;
  let localArchivesMessage = '';
  let localRestoreTargetRoot = '';
  let localRestoreSourceOperationID = '';
  let localRestorePassword = '';
  let localRestorePlan: LocalRestorePlan | null = null;
  let localRestoreLoading = false;
  let localRestoreMessage = '';
  let localRestoreRecoveryItems: LocalRestoreRecoveryItem[] = [];
  let localRestoreRecoveryAvailable = false;
  let localRestoreRecoveryLoading = false;
  let localRestoreRecoveryExecuting = '';
  let copyDestination = '';
  let copyExcludePathsText = '';
  let copyTask: CopyTask | null = null;
  let persistedCopyTasks: CopyTask[] = [];
  let copyLoading = false;
  let copyMessage = '';
  let copyAllowUnavailableProcessProbe = false;
  let copyPollingToken = 0;
  let selectedAssets: FileInventory[] = [];
  let conversationTotal = 0;
  let conversationOffset = 0;
  let conversationSnapshotAt = '';
  let pendingConversationSnapshotAt = '';
  let conversationsLoading = false;
  let conversationsLoadingMore = false;
  let conversationRequestToken = 0;
  let listScrollTop = 0;
  let conversationListElement: HTMLDivElement;
  let listViewportHeight = 640;
  let selectedConversationCount = 0;
  let selectedConversationTokenTotal = 0;
  let allLoadedConversationsSelected = false;
  const conversationPageSize = 200;
  let conversationRowHeight = 96;
  let measuredConversationRowHeight = 96;
  const assetGraphPageSize = 200;

  const statusLabels: Record<string, string> = {
    stable: '稳定',
    active_writing: '正在写入',
    growing: '正在增长',
    pending_jsonl_line: 'JSONL 尾行未完成',
    open_by_process: '被进程打开',
    writer_lock_observed: '发现写入锁',
    stale_lock: '遗留锁',
    sqlite_wal_present: 'SQLite WAL 存在',
    source_changed_during_probe: '探测期间发生变化',
    blocked_for_mutation: '已阻断写入',
    unknown: '未知',
    conflict: '冲突',
    unsupported: '不支持',
    stale: '快照过期',
    active: '活动中',
    needs_review: '需要复核'
  };
  const liveConsistencyLabels: Record<string, string> = {
    stable: '读取稳定',
    active_append: '读取期间追加',
    active_pending_line: '尾行未完成',
    rewrite_conflict: '读取期间重写',
    truncated: '读取期间截断',
    prefix_limited: '达到结果上限'
  };
  const platformCapabilityLabels: Record<string, string> = {
    available: '可用',
    limited: '有限',
    unavailable: '不可用',
    unverified: '未实测'
  };
  const operationPlanStatusLabels: Record<string, string> = {
    prepared: '可执行',
    confirmed: '已确认',
    invalid: '已阻断'
  };
  const pendingStateLabels: Record<string, string> = {
    pending: '已保存意图',
    waiting_for_idle: '等待 Codex 空闲',
    idle_detected: '已检测空闲',
    confirmation_required: '等待重新确认',
    revalidation_failed: '资产已变化',
    ready_for_fixture_execution: '可进入副本执行',
    needs_review: '需要人工复核',
    completed: '已完成',
    cancelled: '已取消'
  };
  const recoveryPhaseLabels: Record<string, string> = {
    prepared: '已准备',
    archived: '已归档',
    isolated: '已隔离',
    source_removed: '源文件已移除',
    control_db_updated: '控制面已更新',
    needs_review: '需要人工复核'
  };
  const recoveryDispositionLabels: Record<string, string> = {
    not_started: '尚未开始 · 可重试',
    retryable: '可重新验证后重试',
    rollbackable: '可安全回滚',
    committed: '已提交',
    rolled_back: '已回滚',
    external_state_unknown: '外部状态未知',
    needs_review: '需要人工复核'
  };
  const workspaceNavigation: Array<{ id: WorkspaceView; label: string; description: string }> = [
    { id: 'home', label: '工作台', description: '工作区数据、最近对话和活动概览' },
    { id: 'sessions', label: '会话库', description: '搜索、阅读和审核对话' },
    { id: 'archives', label: '归档中心', description: '本地与云端副本、恢复和转换' },
    { id: 'pending', label: '待处理', description: '异常、恢复和待判断队列' },
    { id: 'analytics', label: '统计分析', description: 'token、项目和容量趋势' },
    { id: 'maintenance', label: '存储维护', description: '临时文件、缓存和回收站' },
    { id: 'settings', label: '设置与适配器', description: '工作区、归档、更新和兼容性' }
  ];

  $: conversations = report?.conversations ?? [];
  $: technicalEvents = events.filter((event) => event.kind === 'injected_context' || event.role !== 'user' && event.role !== 'assistant');
  $: recentConversations = conversations.slice(0, 6);
  $: homeProjects = (report?.analytics?.projects ?? []).filter((project) => project.name !== '未知项目').slice().sort((left, right) => right.count - left.count || left.name.localeCompare(right.name)).slice(0, 5);
  $: homeTrendPoints = buildHomeTrend(report?.analytics?.daily ?? []);
  $: homeTrendPeak = Math.max(1, ...homeTrendPoints.map((point) => point.total_tokens));
  $: statusCounts = (report?.analytics?.statuses ?? []).reduce<Record<string, number>>((counts, status) => {
    counts[status.name] = status.count;
    return counts;
  }, {});
  $: searchConversationIDs = new Set(searchHits.map((hit) => hit.conversation_id).filter(Boolean));
  $: searchConversationPaths = new Set(searchHits.map((hit) => hit.relative_path).filter(Boolean));
  $: searchIsApplied = Boolean(searchExecutedQuery && searchExecutedQuery === search.trim() && !searching);
  $: filteredConversations = searchIsApplied ? conversations.filter((conversation) => conversationHasSearchHit(conversation)) : conversations;
  $: displayedConversations = projectFamilies(conversations, expandedFamilyIDs, searchIsApplied ? new Set(filteredConversations.map(item => item.id)) : undefined);
  $: visibleStart = Math.max(0, Math.floor(listScrollTop / conversationRowHeight) - 5);
  $: visibleEnd = Math.min(displayedConversations.length, visibleStart + Math.ceil(listViewportHeight / conversationRowHeight) + 10);
  $: visibleConversations = displayedConversations.slice(visibleStart, visibleEnd);
  $: activeConversationFilterCount = activeFilterLabels.length;
  $: selectedConversationCount = selectedConversationIDs.size;
  $: selectedConversationTokenTotal = conversations.reduce((total, conversation) => selectedConversationIDs.has(conversation.id) ? total + conversation.token_usage.total_tokens : total, 0);
  $: allLoadedConversationsSelected = filteredConversations.length > 0 && filteredConversations.every((conversation) => selectedConversationIDs.has(conversation.id));
  $: pendingSelectedOperation = pendingOperations.find((operation) => operation.operation_id === pendingSelectedID);
  $: if (!pendingDrawerOpen) {
    pendingArchivePassword = '';
    pendingConfirmationCode = '';
    pendingPreviewCode = '';
    pendingReviewed = false;
  }
  $: pendingOpenCount = pendingOperations.filter((operation) => !['completed', 'cancelled'].includes(operation.state)).length;
  $: filteredPendingOperations = pendingOperations.filter((operation) => {
    const stateMatches = pendingFilter === 'all' || (pendingFilter === 'history' ? ['completed', 'cancelled'].includes(operation.state) : pendingFilter ? operation.state === pendingFilter : !['completed', 'cancelled'].includes(operation.state));
    return stateMatches && pendingMatchesQuery([operation.operation_id, operation.conversation_id, pendingConversation(operation)?.title, operationSummary(operation.strategy), ...(operation.block_reasons ?? [])]);
  });
  $: filteredRecoveryItems = recoveryItems.filter((item) => (!pendingFilter || (pendingFilter === 'rollback' ? item.can_rollback : item.external_state_unknown || !item.can_rollback)) && pendingMatchesQuery([item.operation_id, item.conversation_id, item.error_message, item.reason]));
  $: projectFilters = (report?.analytics?.projects ?? []).slice().sort((left, right) => right.count - left.count || left.name.localeCompare(right.name));
  $: visibleProjectFilters = projectFilters.filter((project) => project.name.toLocaleLowerCase().includes(projectSearch.trim().toLocaleLowerCase()));
  $: savedFilters = savedFilterStore[savedFilterScope()] ?? [];
  $: activeWorkspaceLabel = workspaceNavigation.find((item) => item.id === activeWorkspace)?.label || '工作台';
  $: liveModeLabel = report?.mode === 'live_best_effort' ? '活动读取' : report?.mode === 'live_read' ? '活动读取' : '只读读取';
  $: activeFilterLabels = [
    { key: 'search', label: '正文', value: searchExecutedQuery },
    { key: 'status', label: '状态', value: statusFilter !== '全部状态' ? statusLabels[statusFilter] || statusFilter : '' },
    { key: 'query', label: '关键词', value: conversationQuery.trim() },
    { key: 'cwd', label: cwdExact ? '精确项目' : '项目', value: cwdFilter.trim() },
    { key: 'model', label: modelExact ? '精确模型' : '模型', value: modelFilter.trim() },
    { key: 'time', label: '时间', value: fromFilter || toFilter ? `${fromFilter || '不限'} – ${toFilter || '不限'}` : '' },
    { key: 'diagnostics', label: '诊断', value: diagnosticsFilter ? diagnosticsFilter === 'true' ? '有诊断' : '无诊断' : '' },
    { key: 'tokens', label: 'tokens', value: minTokensFilter || maxTokensFilter ? `${minTokensFilter || '不限'} – ${maxTokensFilter || '不限'}` : '' },
    { key: 'bytes', label: '字节', value: minBytesFilter || maxBytesFilter ? `${minBytesFilter || '不限'} – ${maxBytesFilter || '不限'}` : '' }
  ].filter((item) => item.value) as typeof activeFilterLabels;
  $: searchHasNoResults = Boolean(searchExecutedQuery && searchExecutedQuery === search.trim() && !searching && searchHits.length === 0);
  $: searchHasNoVisibleResults = Boolean(searchIsApplied && searchHits.length > 0 && displayedConversations.length === 0);
  $: operationChosen = operationOptions.archive || operationOptions.trash || operationOptions.permanent_delete;
  $: operationSelectionError = archiveLocationMissing(operationOptions) ? '归档至少选择一个保存位置' : '';
  $: operationExecutable = Boolean(lifecycleWritesEnabled && !executionTasks.some(task => task.state === 'running') && impactPreview?.preview_id && !previewLoading && impactPreview?.plan?.status === 'prepared' && !impactPreview.blocked && (!operationOptions.encrypted || operationArchivePassword.trim()));
  $: batchOperationExecutable = Boolean(lifecycleWritesEnabled && !executionTasks.some(task => task.state === 'running') && batchOperationPreview?.preview_id && batchOperationPreview && !batchOperationPreview.blocked && batchOperationPreview.items.length && batchOperationPreview.items.every((item) => !item.blocked && !item.completed && !item.result && item.plan?.status === 'prepared') && (!operationOptions.encrypted || operationArchivePassword.trim()));
  $: batchSharedError = batchOperationPreview?.items.length && batchOperationPreview.items.every(item => item.blocked && item.error === batchOperationPreview?.items[0].error) ? batchOperationPreview.items[0].error || '' : '';
  // 隐藏面板会测到零高度，保留上一次有效行高，避免虚拟列表出现除零和空白。
  $: if (measuredConversationRowHeight > 0) conversationRowHeight = measuredConversationRowHeight;

  onMount(() => {
    applyBrowserRoute();
    loadSavedFilterStore();
    loadDensity();
    void startApplicationWhenAuthorized();
    const handlePopstate = () => applyBrowserRoute();
    const handleFocus = () => {
      if (applicationStarted && inventoryReady) void loadExecutionTasks();
      if (applicationStarted && activeWorkspace === 'settings' && settingsCategory === 'remote') void loadGitHubStatus();
    };
    const handleVisibility = () => { if (document.visibilityState === 'visible') handleFocus(); };
    const handleGlobalKeydown = (event: KeyboardEvent) => {
      if (document.querySelector('dialog[open]')) return;
      if (isEditableTarget(event.target)) {
        if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') {
          event.preventDefault();
          document.querySelector<HTMLInputElement>('.global-search input')?.focus();
        }
        return;
      }
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') {
        event.preventDefault();
        document.querySelector<HTMLInputElement>('.global-search input')?.focus();
      } else if ((event.target as Element | null)?.closest('button, a, summary, [role="button"]') && !(['j', 'k', 'ArrowDown', 'ArrowUp'].includes(event.key) && (event.target as Element).closest('.conversation-row'))) {
        // 会话行支持方向键，其他控件继续保留原生键盘行为。
        return;
      } else if (event.key === '/') {
        event.preventDefault();
        document.querySelector<HTMLInputElement>('.global-search input')?.focus();
      } else if ((event.key === 'j' || event.key === 'ArrowDown') && activeWorkspace === 'sessions') {
        event.preventDefault();
        void moveConversationSelection(1);
      } else if ((event.key === 'k' || event.key === 'ArrowUp') && activeWorkspace === 'sessions') {
        event.preventDefault();
        void moveConversationSelection(-1);
      } else if (event.key === 'Enter' && activeWorkspace === 'sessions' && compactPane === 'sessions' && selectedConversation) {
        event.preventDefault();
        navigateToRoute({ page: 'session', conversationID: selectedConversation.id }, false);
      } else if (event.key === 'Escape' && activeWorkspace === 'sessions' && compactPane === 'detail') {
        selectWorkspaceView('sessions');
      } else if (event.key === 'Escape' && conversationFiltersOpen) {
        conversationFiltersOpen = false;
      }
    };
    window.addEventListener('keydown', handleGlobalKeydown);
    window.addEventListener('popstate', handlePopstate);
    window.addEventListener('focus', handleFocus);
    document.addEventListener('visibilitychange', handleVisibility);
    return () => {
      if (githubPollTimer) clearTimeout(githubPollTimer);
      if (executionPollTimer) clearTimeout(executionPollTimer);
      window.removeEventListener('keydown', handleGlobalKeydown);
      window.removeEventListener('popstate', handlePopstate);
      window.removeEventListener('focus', handleFocus);
      document.removeEventListener('visibilitychange', handleVisibility);
    };
  });

  function selectWorkspaceView(view: WorkspaceView) {
    const route: AppRoute = view === 'settings' ? { page: 'settings', category: settingsCategory } : { page: view };
    navigateToRoute(route);
  }

  function navigateToRoute(route: AppRoute, restoreConversation = true, replace = false) {
    if (route.page === 'sessions') detailView = 'reading';
    const query = route.page === 'sessions' || route.page === 'session'
      ? sessionViewSearch(currentFilterSnapshot(), route.page === 'session' ? detailView : 'reading') : route.page === 'pending' ? pendingViewSearch() : '';
    const targetParams = new URLSearchParams(query);
    if (route.page === 'session' && detailView === 'reading' && readerLocation) {
      targetParams.set('read_path', readerLocation.path); targetParams.set('read_start', String(readerLocation.start));
      if (readerLocation.anchor) targetParams.set('read_anchor', readerLocation.anchor);
      if (readerLocation.query) targetParams.set('read_q', readerLocation.query);
      if (readerLocation.source) targetParams.set('read_source', readerLocation.source);
    }
    const path = `${routePath(route)}${targetParams.size ? `?${targetParams}` : ''}`;
    if (`${window.location.pathname}${window.location.search}` !== path) {
      if (replace) window.history.replaceState({}, '', path);
      else window.history.pushState({}, '', path);
    }
    applyRoute(route, restoreConversation);
  }

  function applyBrowserRoute() {
    const route = parseRoute(window.location.pathname);
    if (route.page !== 'not-found') {
      const canonicalPath = routePath(route);
      if (canonicalPath !== window.location.pathname) {
        window.history.replaceState(window.history.state, '', `${canonicalPath}${window.location.search}${window.location.hash}`);
      }
    }
    const isSessionRoute = route.page === 'sessions' || route.page === 'session';
    const view = parseSessionView(window.location.search);
    const filtersChanged = isSessionRoute && JSON.stringify(currentFilterSnapshot()) !== JSON.stringify(view.filters);
    if (isSessionRoute) {
      applyFilterSnapshot(view.filters);
      detailView = view.detail;
      readerLocation = route.page === 'session' ? readerLocationFromURL(window.location.search) : null;
    }
    if (route.page === 'pending') {
      const params = new URLSearchParams(window.location.search);
      const queue = params.get('queue');
      pendingTab = queue === 'operations' || queue === 'recovery' ? queue : 'conversations';
      pendingQuery = params.get('q')?.trim() || '';
      pendingSearchDraft = pendingQuery;
      pendingFilter = params.get('state') || '';
      const states = pendingTab === 'conversations' ? Object.keys(statusLabels) : pendingTab === 'operations' ? [...Object.keys(pendingStateLabels), 'all', 'history'] : ['rollback', 'review'];
      if (!states.includes(pendingFilter)) pendingFilter = '';
    }
    applyRoute(route, !filtersChanged);
    if (inventoryReady && filtersChanged) void reloadSessionResults();
    else if (route.page === 'session' && selectedConversation?.id === route.conversationID && detailView === 'technical') showTechnicalDetails(false);
  }

  function applyRoute(route: AppRoute, restoreConversation = true) {
    const previousConversationID = routeConversationID;
    routeRestoreToken += 1;
    conversationFiltersOpen = false;
    batchPanelOpen = false;
    reviewPanelOpen = false;
    pendingDrawerOpen = false;
    pendingConfirmationCode = '';
    pendingArchivePassword = '';
    pendingPreviewCode = '';
    pendingReviewed = false;
    routeNotFoundPath = route.page === 'not-found' ? route.pathname : '';
    routeConversationID = route.page === 'session' ? route.conversationID : '';
    if (route.page === 'settings') settingsCategory = route.category;
    activeWorkspace = route.page === 'session' || route.page === 'not-found' ? 'home' : route.page;

    if (route.page === 'session') {
      activeWorkspace = 'sessions';
      compactPane = 'detail';
      if (previousConversationID !== route.conversationID) {
        routeConversationError = '';
        routeConversationLoading = !inventoryReady;
      }
      if (restoreConversation && inventoryReady && selectedConversation?.id !== route.conversationID) {
        void restoreConversationFromRoute(route.conversationID);
      }
    } else {
      routeConversationLoading = false;
      routeConversationError = '';
      if (route.page === 'sessions' || route.page === 'pending') compactPane = 'sessions';
      else compactPane = 'workspace';
    }
    if (route.page === 'pending' && inventoryReady) void refreshPendingWorkspace();
  }

  function savedFilterScope() {
    return currentWorkspace?.id || currentWorkspace?.root || report?.root || 'default';
  }

  function loadDensity() {
    try {
      const stored = window.localStorage.getItem('codex-chronicle.density.v1');
      if (stored === 'comfortable' || stored === 'compact' || stored === 'reading') density = stored;
    } catch {
      density = 'comfortable';
    }
  }

  function setDensity(value: Density) {
    density = value;
    densityStorageError = false;
    try {
      window.localStorage.setItem('codex-chronicle.density.v1', value);
    } catch {
      densityStorageError = true;
      // 本地存储不可用时仍保留当前页面的显示密度。
    }
  }

  function isEditableTarget(target: EventTarget | null) {
    const element = target as HTMLElement | null;
    return Boolean(element && typeof element.closest === 'function' && element.closest('input, textarea, select, [contenteditable="true"]'));
  }

  async function moveConversationSelection(delta: number) {
    const candidates = displayedConversations;
    if (!candidates.length) return;
    const currentIndex = selectedConversation ? candidates.findIndex((item) => item.id === selectedConversation?.id) : -1;
    const nextIndex = Math.min(candidates.length - 1, Math.max(0, currentIndex < 0 ? (delta > 0 ? 0 : candidates.length - 1) : currentIndex + delta));
    const next = candidates[nextIndex];
    if (!next || next.id === selectedConversation?.id) return;
    if (conversationListElement) {
      const rowTop = nextIndex * conversationRowHeight;
      const rowBottom = rowTop + conversationRowHeight;
      if (rowTop < conversationListElement.scrollTop) conversationListElement.scrollTop = rowTop;
      else if (rowBottom > conversationListElement.scrollTop + conversationListElement.clientHeight) {
        conversationListElement.scrollTop = rowBottom - conversationListElement.clientHeight;
      }
      listScrollTop = conversationListElement.scrollTop;
    }
    await chooseConversation(next);
    if (selectedConversation?.id === next.id) conversationListElement?.querySelector<HTMLButtonElement>('.conversation-row[aria-current="true"]')?.focus({ preventScroll: true });
  }

  function openConversationFilters() {
    filterDraft = currentFilterSnapshot();
    conversationFiltersOpen = true;
  }

  function applyFilterSnapshot(filters: FilterSnapshot) {
    search = filters.search;
    searchDraft = filters.search;
    statusFilter = filters.status;
    conversationQuery = filters.query;
    cwdFilter = filters.cwd;
    modelFilter = filters.model;
    modelExact = filters.modelExact || '';
    cwdExact = filters.cwdExact || '';
    fromFilter = filters.from;
    toFilter = filters.to;
    minTokensFilter = filters.minTokens;
    maxTokensFilter = filters.maxTokens;
    minBytesFilter = filters.minBytes;
    maxBytesFilter = filters.maxBytes;
    diagnosticsFilter = filters.diagnostics;
  }

  async function reloadSessionResults() {
    errorMessage = '';
    searchRequestToken += 1;
    searching = false;
    searchHits = [];
    searchLimited = false;
    searchExecutedQuery = '';
    const snapshot = JSON.stringify(currentFilterSnapshot());
    await loadConversations(true);
    if (snapshot === JSON.stringify(currentFilterSnapshot()) && search.trim()) await runSearch(true, false);
  }

  async function removeSessionFilter(key: typeof activeFilterLabels[number]['key']) {
    const filters = currentFilterSnapshot();
    if (key === 'time') { filters.from = ''; filters.to = ''; }
    else if (key === 'tokens') { filters.minTokens = ''; filters.maxTokens = ''; }
    else if (key === 'bytes') { filters.minBytes = ''; filters.maxBytes = ''; }
    else filters[key] = key === 'status' ? '全部状态' : '';
    if (key === 'model') filters.modelExact = '';
    if (key === 'cwd') filters.cwdExact = '';
    applyFilterSnapshot(filters);
    navigateToRoute({ page: 'sessions' });
    await reloadSessionResults();
  }

  function conversationHasSearchHit(conversation: ConversationSummary) {
    return searchConversationIDs.has(conversation.id) || conversation.rollout_paths.some((path) => searchConversationPaths.has(path));
  }

  function formatPath(value?: string, keep = 2) {
    const raw = (value || '').trim();
    if (!raw) return '未关联项目';
    const normalized = raw.replaceAll('\\', '/');
    const parts = normalized.split('/').filter(Boolean);
    if (parts.length <= keep + 1) return normalized;
    return `${parts.slice(0, 1).join('/')}/…/${parts.slice(-keep).join('/')}`;
  }

  function compactID(value?: string, keep = 8) {
    const raw = (value || '').trim();
    if (!raw || raw.length <= keep * 2 + 1) return raw || '未知';
    return `${raw.slice(0, keep)}…${raw.slice(-keep)}`;
  }

  async function copyText(value: string | undefined, label: string) {
    const text = (value || '').trim();
    if (!text) return;
    try {
      if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(text);
      } else {
        const input = document.createElement('textarea');
        input.value = text;
        input.style.position = 'fixed';
        input.style.opacity = '0';
        document.body.appendChild(input);
        input.select();
        document.execCommand('copy');
        input.remove();
      }
      copiedValue = label;
      window.setTimeout(() => {
        if (copiedValue === label) copiedValue = '';
      }, 1400);
    } catch {
      copiedValue = '';
    }
  }

  function loadSavedFilterStore() {
    try {
      const raw = window.localStorage.getItem('codex-chronicle.saved-filters.v1');
      if (!raw) return;
      const parsed = JSON.parse(raw) as Record<string, SavedFilter[]>;
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return;
      savedFilterStore = Object.fromEntries(Object.entries(parsed).map(([scope, filters]) => [
        scope,
        Array.isArray(filters) ? filters.filter((filter) => filter && typeof filter.id === 'string' && typeof filter.name === 'string' && filter.filters) : []
      ]));
    } catch {
      savedFilterStore = {};
    }
  }

  function persistSavedFilterStore() {
    try {
      window.localStorage.setItem('codex-chronicle.saved-filters.v1', JSON.stringify(savedFilterStore));
    } catch {
      // 浏览器存储不可用时，筛选仍保留在当前页面内。
    }
  }

  function currentFilterSnapshot(): FilterSnapshot {
    return {
      search,
      status: statusFilter,
      query: conversationQuery,
      cwd: cwdFilter,
      model: modelFilter,
      modelExact,
      cwdExact,
      from: fromFilter,
      to: toFilter,
      minTokens: minTokensFilter,
      maxTokens: maxTokensFilter,
      minBytes: minBytesFilter,
      maxBytes: maxBytesFilter,
      diagnostics: diagnosticsFilter
    };
  }

  function savedFilterDescription(filter: SavedFilter) {
    const labels = [
      filter.filters.search.trim() ? `关键词：${filter.filters.search.trim()}` : '',
      filter.filters.status !== '全部状态' ? filter.filters.status : '',
      filter.filters.cwd.trim() ? `项目：${filter.filters.cwd.trim()}` : '',
      filter.filters.model.trim() ? `模型：${filter.filters.model.trim()}` : ''
    ].filter(Boolean);
    return labels.length ? labels.slice(0, 2).join(' · ') : '全部对话';
  }

  function saveCurrentFilter() {
    const name = savedFilterName.trim() || `筛选 ${savedFilters.length + 1}`;
    const scope = savedFilterScope();
    const next = [{ id: `${Date.now()}-${Math.random().toString(16).slice(2)}`, name, filters: currentFilterSnapshot() }, ...savedFilters].slice(0, 20);
    savedFilterStore = { ...savedFilterStore, [scope]: next };
    savedFilterName = '';
    persistSavedFilterStore();
  }

  async function applySavedFilter(filter: SavedFilter) {
    applyFilterSnapshot(filter.filters);
    selectWorkspaceView('sessions');
    await reloadSessionResults();
  }

  function deleteSavedFilter(filterID: string) {
    const scope = savedFilterScope();
    savedFilterStore = { ...savedFilterStore, [scope]: savedFilters.filter((filter) => filter.id !== filterID) };
    persistSavedFilterStore();
  }

  function openConversationFromWorkspace(conversation: ConversationSummary) {
    void chooseConversation(conversation);
  }

  async function drillIntoAnalytics(kind: 'model' | 'project', value: string) {
    modelExact = '';
    cwdExact = '';
    if (kind === 'model') {
      modelFilter = value;
      cwdFilter = '';
    } else {
      cwdFilter = value;
      modelFilter = '';
    }
    selectWorkspaceView('sessions');
    await reloadSessionResults();
  }

  async function openAnalyticsConversations(selection: AnalyticsSelection) {
    applyFilterSnapshot({
      ...emptySessionFilters(), from: selection.from, to: selection.to,
      model: selection.model, modelExact: selection.model ? 'true' : '',
      cwd: selection.project, cwdExact: selection.project ? 'true' : '',
      status: selection.status || '全部状态'
    });
    selectWorkspaceView('sessions');
    await reloadSessionResults();
  }

  async function startApplicationWhenAuthorized() {
    if (!(await initializeAuth())) return;
    await startApplication();
  }

  async function startApplication() {
    if (applicationStarted) return;
    applicationStarted = true;
    await loadWorkspaces();
    void loadWorkspaceDiscovery();
    await Promise.all([loadInventory(), loadTrashDetection(), loadPlatformCapabilities(), loadPendingOperations(), loadRemoteSetup()]);
    await loadExecutionTasks();
    await loadRecoveryItems();
    await loadLocalArchives();
    await loadLocalRestoreRecoveryItems();
    await loadCopyPlans();
    await loadUpdateStatus();
    await loadUpdatePreflight();
  }

  async function initializeAuth(): Promise<boolean> {
    try {
      sessionToken = sessionStorage.getItem('codex-chronicle-session') || '';
    } catch {
      sessionToken = '';
    }
    try {
      const response = await apiFetch('/api/v1/auth/status');
      if (response.status === 404) {
        clearSessionToken();
        authConfigured = false;
        authRequired = false;
        return true;
      }
      if (!response.ok) throw new Error(`读取认证状态失败（HTTP ${response.status}）`);
      const body = (await response.json()) as { auth?: LocalAuthStatus };
      authConfigured = body.auth?.enabled ?? true;
      authBound = body.auth?.bound ?? false;
      authBindingExpiresAt = body.auth?.binding_expires_at ?? '';
      if (!authConfigured) return true;
      if (!authBound || !sessionToken) {
        authRequired = true;
        authMessage = authBound ? '请输入本地账户密码登录。' : '请输入 daemon 启动时显示的一次性绑定码。';
        return false;
      }
      const capabilityResponse = await apiFetch('/api/v1/capabilities');
      if (capabilityResponse.status === 401) {
        clearSessionToken();
        authRequired = true;
        authMessage = '会话已失效，请重新登录。';
        return false;
      }
      if (!capabilityResponse.ok) throw new Error(`验证会话失败（HTTP ${capabilityResponse.status}）`);
      authRequired = false;
      return true;
    } catch (error) {
      if (error instanceof TypeError) {
        authConfigured = false;
        authRequired = false;
        errorMessage = '无法连接本地 daemon';
        return true;
      }
      authMessage = error instanceof Error ? error.message : '无法读取认证状态';
      authRequired = true;
      return false;
    }
  }

  function clearSessionToken() {
    sessionToken = '';
    try {
      sessionStorage.removeItem('codex-chronicle-session');
    } catch {
      // 浏览器禁用 sessionStorage 时仍允许本次页面会话继续工作。
    }
  }

  async function bindRemote() {
    authLoading = true;
    authMessage = '';
    try {
      const response = await apiFetch('/api/v1/auth/bind', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ code: authCode.trim(), device: authDevice.trim(), password: authPassword })
      });
      const body = (await response.json()) as { error?: string; session?: { token?: string } };
      if (!response.ok || !body.session?.token) throw new Error(body.error || `绑定失败（HTTP ${response.status}）`);
      await activateSession(body.session.token);
    } catch (error) {
      authMessage = error instanceof Error ? error.message : '远程绑定失败';
    } finally {
      authLoading = false;
    }
  }

  async function loginRemote() {
    authLoading = true;
    authMessage = '';
    try {
      const response = await apiFetch('/api/v1/auth/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ device: authDevice.trim(), password: authPassword })
      });
      const body = (await response.json()) as { error?: string; session?: { token?: string } };
      if (!response.ok || !body.session?.token) throw new Error(body.error || `登录失败（HTTP ${response.status}）`);
      await activateSession(body.session.token);
    } catch (error) {
      authMessage = error instanceof Error ? error.message : '登录失败';
    } finally {
      authLoading = false;
    }
  }

  async function reissueBinding() {
    authLoading = true;
    authMessage = '';
    try {
      const response = await apiFetch('/api/v1/auth/reissue-binding', { method: 'POST' });
      const body = (await response.json()) as { error?: string; binding_code?: string; expires_at?: string };
      if (!response.ok || !body.binding_code) throw new Error(body.error || `重新生成绑定码失败（HTTP ${response.status}）`);
      authCode = body.binding_code;
      authBindingExpiresAt = body.expires_at ?? '';
      authMessage = '已生成新的绑定码，请确认当前请求来自本机回环地址。';
    } catch (error) {
      authMessage = error instanceof Error ? error.message : '重新生成绑定码失败';
    } finally {
      authLoading = false;
    }
  }

  function bindingExpired() {
    return Boolean(authBindingExpiresAt && Date.parse(authBindingExpiresAt) <= Date.now());
  }

  async function activateSession(token: string) {
    sessionToken = token;
    try {
      sessionStorage.setItem('codex-chronicle-session', token);
    } catch {
      // 页面仍可使用内存中的令牌；刷新后需要重新绑定或登录。
    }
    authRequired = false;
    authMessage = '';
    await startApplication();
  }

  async function apiFetch(input: RequestInfo | URL, init: RequestInit = {}): Promise<Response> {
    const headers = new Headers(init.headers);
    if (sessionToken) headers.set('Authorization', `Bearer ${sessionToken}`);
    return window.fetch(input, { ...init, headers });
  }

  async function loadRemoteSetup() {
    await loadRemoteCapabilities();
    await loadGitHubStatus();
    if (remoteProvider?.id === 'github') {
      await Promise.all([loadGitHubArchives(), loadGitHubRepositories(), loadGitHubArchiveTarget()]);
    } else {
      githubArchiveTarget = null;
      githubRepositories = [];
      await loadGitHubArchives();
    }
  }

  async function loadRemoteCapabilities() {
    const sourceRoot = currentWorkspace?.root;
    lifecycleWritesEnabled = false;
    lifecycleFixtureOnly = false;
    lifecycleRestricted = false;
    lifecycleRealConversations = false;
    try {
      const response = await apiFetch('/api/v1/capabilities');
      if (!response.ok) throw new Error(`读取远端 provider 能力失败（HTTP ${response.status}）`);
      const body = (await response.json()) as { write_enabled?: boolean; fixture_only?: boolean; restricted_real_execution?: { enabled: boolean }; real_conversation_execution?: { enabled: boolean }; remote_archive_provider_ready?: boolean; remote_archive?: RemoteArchiveProviderInfo; summary?: { providers?: string[] } };
      if (sourceRoot !== currentWorkspace?.root) return;
      lifecycleRestricted = body.restricted_real_execution?.enabled ?? false;
      lifecycleRealConversations = body.real_conversation_execution?.enabled ?? false;
      lifecycleWritesEnabled = Boolean(body.write_enabled || lifecycleRestricted || lifecycleRealConversations);
      if (lifecycleRestricted && operationOptions.archive) {
        operationOptions = { archive: false, archive_local: false, archive_remote: false, encrypted: false, trash: true, permanent_delete: false };
      }
      lifecycleFixtureOnly = lifecycleWritesEnabled && (body.fixture_only ?? false);
      remoteProvider = body.remote_archive ?? null;
      remoteProviderConfigured = body.remote_archive_provider_ready ?? false;
      remoteTargetRequired = remoteProvider?.target_required ?? false;
      summaryProviderIDs = body.summary?.providers?.length ? body.summary.providers : ['local-deterministic'];
      if (!summaryProviderIDs.includes(summaryProviderID)) summaryProviderID = summaryProviderIDs[0];
    } catch (error) {
      if (sourceRoot !== currentWorkspace?.root) return;
      githubArchiveMessage = error instanceof Error ? error.message : '无法读取远端 provider 能力';
    }
  }

  async function loadWorkspaces() {
    try {
      const response = await apiFetch('/api/v1/workspaces');
      if (!response.ok) throw new Error(`读取工作区失败（HTTP ${response.status}）`);
      const body = (await response.json()) as WorkspaceResponse;
      workspaces = body.workspaces ?? [];
      currentWorkspace = body.current?.root ? body.current : workspaces.find((item) => item.id === body.current_id) ?? null;
      workspacePersisted = body.persisted === true;
      workspaceRegistryPath = body.registry_path ?? '';
      workspaceServiceAddress = window.location.origin;
      workspaceMessage = '';
    } catch (error) {
      workspaceMessage = error instanceof Error ? error.message : '无法读取工作区';
    }
  }

  async function loadWorkspaceDiscovery() {
    workspaceDiscoveryLoading = true;
    workspaceDiscoveryMessage = '';
    try {
      const response = await apiFetch('/api/v1/workspaces/discovery');
      if (response.status === 404) throw new Error('此服务版本尚不支持自动识别，请更新并重启服务，或手动添加目录。');
      if (!response.ok) throw new Error(`识别 Codex 目录失败（HTTP ${response.status}）`);
      const body = await response.json() as { candidates: CodexHomeCandidate[] };
      workspaceCandidates = body.candidates ?? [];
    } catch (error) {
      workspaceDiscoveryMessage = error instanceof Error ? error.message : '无法识别 Codex 目录';
    } finally {
      workspaceDiscoveryLoading = false;
    }
  }

  async function useDiscoveredWorkspace(candidate: CodexHomeCandidate) {
    if (candidate.workspace_id) await selectWorkspace(candidate.workspace_id);
    else await addWorkspace(candidate);
  }

  async function loadTrashDetection() {
    trashDetectionLoading = true;
    trashDetectionMessage = '';
    try {
      const response = await apiFetch('/api/v1/platform/trash');
      const body = (await response.json()) as { trash?: TrashDetection; settings?: { custom_path?: string }; error?: string };
      if (!response.ok || !body.trash) throw new Error(body.error || `读取回收站探测失败（HTTP ${response.status}）`);
      trashDetection = body.trash;
      trashCustomPath = body.settings?.custom_path || '';
      trashSettingsMessage = '';
      trashSettingsError = false;
    } catch (error) {
      trashDetection = null;
      trashDetectionMessage = error instanceof Error ? error.message : '无法读取回收站探测结果';
    } finally {
      trashDetectionLoading = false;
    }
  }

  async function loadPlatformCapabilities() {
    platformCapabilitiesLoading = true;
    platformCapabilitiesMessage = '';
    try {
      const response = await apiFetch('/api/v1/platform/capabilities');
      const body = (await response.json()) as Partial<PlatformCapabilitiesResponse> & { error?: string };
      if (!response.ok || !body.platform) throw new Error(body.error || `读取平台能力失败（HTTP ${response.status}）`);
      platformCapabilities = body.platform;
    } catch (error) {
      platformCapabilities = null;
      platformCapabilitiesMessage = error instanceof Error ? error.message : '无法读取平台能力';
    } finally {
      platformCapabilitiesLoading = false;
    }
  }

  async function saveTrashSettings(customPath = trashCustomPath) {
    trashSettingsLoading = true;
    trashSettingsMessage = '';
    try {
      const response = await apiFetch('/api/v1/platform/trash/settings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ custom_path: customPath.trim() })
      });
      const body = (await response.json()) as { trash?: TrashDetection; settings?: { custom_path?: string }; error?: string };
      if (!response.ok || !body.trash) throw new Error(body.error || `保存回收站设置失败（HTTP ${response.status}）`);
      trashDetection = body.trash;
      trashCustomPath = body.settings?.custom_path || '';
      trashSettingsMessage = trashDetection.configured ? '自定义回收站路径已保存' : '已恢复系统默认回收站路径';
      trashSettingsError = false;
    } catch (error) {
      trashSettingsMessage = error instanceof Error ? error.message : '无法保存回收站设置';
      trashSettingsError = true;
    } finally {
      trashSettingsLoading = false;
    }
  }

  async function readOperationDestinations() {
    const root = currentWorkspace?.root || report?.root || '';
    const response = await apiFetch('/api/v1/operations/destinations');
    const body = await response.json() as OperationDestinations & { error?: string };
    if (!response.ok) throw new Error(body.error || '无法读取处理目的地');
    if (root !== (currentWorkspace?.root || report?.root || '') || body.source_root !== root) throw new Error('工作区已变化，请重新检查目的地');
    destinationStatus = body;
    return body;
  }

  async function openDestinationCheck(needsArchive = false, needsTrash = true) {
    destinationNeedsArchive = needsArchive;
    destinationNeedsTrash = needsTrash;
    destinationOpen = true;
    destinationBusy = true;
    destinationStatus = null;
    destinationMessage = '';
    try { await readOperationDestinations(); }
    catch (error) { destinationMessage = error instanceof Error ? error.message : '无法读取处理目的地'; }
    finally { destinationBusy = false; }
  }

  async function checkExecutionDestinations(options: { archive: boolean; archive_local?: boolean; archive_remote?: boolean; trash: boolean } = effectiveOperationOptions()) {
    if (!(options.archive && options.archive_local) && !options.trash) return true;
    if (destinationBusy) return false;
    destinationBusy = true;
    destinationNeedsArchive = Boolean(options.archive && options.archive_local);
    destinationNeedsTrash = options.trash;
    try {
      const status = await readOperationDestinations();
      if (destinationsReady(status, options, currentWorkspace?.root || report?.root || '')) return true;
      destinationMessage = '首次执行前请确认目的地。完成检查后，返回影响预览再次确认执行。';
      destinationOpen = true;
    } catch (error) {
      destinationStatus = null;
      destinationMessage = error instanceof Error ? error.message : '无法读取处理目的地';
      destinationOpen = true;
    } finally { destinationBusy = false; }
    return false;
  }

  async function sendTrashProbe() {
    if (destinationBusy || !destinationStatus) return;
    destinationBusy = true;
    destinationMessage = '';
    try {
      const response = await apiFetch('/api/v1/platform/trash/probe', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ destination_key: destinationStatus.key }) });
      const body = await response.json() as { error?: string };
      await readOperationDestinations();
      if (!response.ok) throw new Error(body.error || '测试文件移动失败');
      destinationMessage = '测试文件已发送。请打开系统回收站或上方目录，找到样本后点击“我已找到测试文件”。';
    } catch (error) { destinationMessage = error instanceof Error ? error.message : '回收站测试失败'; }
    finally { destinationBusy = false; }
  }

  async function confirmDestination(found?: boolean, archive = false) {
    if (destinationBusy || !destinationStatus) return;
    destinationBusy = true;
    destinationMessage = '';
    try {
      const response = await apiFetch('/api/v1/operations/destinations/confirm', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ destination_key: destinationStatus.key, ...(archive ? { archive_path: destinationStatus.archive_path } : { probe_id: destinationStatus.probe?.id, found }) }) });
      const body = await response.json() as OperationDestinations & { error?: string };
      if (!response.ok) throw new Error(body.error || '目的地确认失败');
      destinationStatus = body;
      destinationMessage = found === false ? '尚未确认位置。可以更改路径或重新发送测试文件。' : '目的地已确认。关闭此窗口后，请重新核对影响预览再执行。';
    } catch (error) { destinationMessage = error instanceof Error ? error.message : '目的地确认失败'; }
    finally { destinationBusy = false; }
  }

  async function correctTrashDestination(path: string) {
    if (destinationBusy) return;
    destinationBusy = true;
    try {
      await saveTrashSettings(path);
      if (trashSettingsError) throw new Error(trashSettingsMessage);
      await readOperationDestinations();
      invalidateOperationPreview();
      invalidateBatchOperationPreview();
      destinationMessage = '位置已更新。请重新发送测试文件并确认。';
    } catch (error) { destinationMessage = error instanceof Error ? error.message : '无法更改回收站位置'; }
    finally { destinationBusy = false; }
  }

  async function loadCopyPlans() {
    try {
      const response = await apiFetch('/api/v1/copies');
      if (!response.ok) throw new Error(`读取副本计划失败（HTTP ${response.status}）`);
      const body = (await response.json()) as { prepared_tasks?: CopyTask[] };
      persistedCopyTasks = body.prepared_tasks ?? [];
    } catch (error) {
      copyMessage = error instanceof Error ? error.message : '无法读取副本计划';
    }
  }

  async function loadUpdateStatus() {
    try {
      const response = await apiFetch('/api/v1/update/status');
      if (!response.ok) throw new Error(`读取更新状态失败（HTTP ${response.status}）`);
      const body = (await response.json()) as { available?: boolean; status?: UpdateStatus; reason?: string };
      updateStatus = body.available && body.status ? body.status : null;
      onlineUpdate = null;
      if (!body.available && body.reason) updateMessage = body.reason;
    } catch (error) {
      updateStatus = null;
      updateMessage = error instanceof Error ? error.message : '无法读取更新状态';
    }
  }

  async function checkOnlineUpdate() {
    if (!updateStatus?.online_configured || updateLoading) return;
    updateLoading = true;
    updateMessage = '';
    try {
      const response = await apiFetch('/api/v1/update/check');
      const body = (await response.json()) as { check?: OnlineCheck; available?: boolean; error?: string };
      if (!response.ok || !body.check) throw new Error(body.error || `在线版本检查失败（HTTP ${response.status}）`);
      onlineUpdate = body.check;
      updateMessage = body.check.available && body.check.release ? `发现 ${body.check.release.version} 可用更新，请下载并预检。` : '当前已没有更高版本。';
    } catch (error) {
      onlineUpdate = null;
      updateMessage = error instanceof Error ? error.message : '在线版本检查失败';
    } finally {
      updateLoading = false;
    }
  }

  async function prepareOnlineUpdate() {
    if (!onlineUpdate?.available || !onlineUpdate.release || updateLoading) return;
    updateLoading = true;
    updateMessage = '';
    try {
      const response = await apiFetch('/api/v1/update/prepare-online', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}'
      });
      const body = (await response.json()) as { plan?: UpdatePlan; error?: string };
      if (!response.ok || !body.plan) throw new Error(body.error || `在线更新预检失败（HTTP ${response.status}）`);
      updatePlan = body.plan;
      updateMessage = `已下载并生成 ${body.plan.version} 版本计划，请审阅后确认切换。`;
      await loadUpdateStatus();
    } catch (error) {
      updateMessage = error instanceof Error ? error.message : '在线更新预检失败';
    } finally {
      updateLoading = false;
    }
  }

  async function loadUpdatePreflight() {
    try {
      const response = await apiFetch('/api/v1/health/preflight');
      if (!response.ok) throw new Error(`读取更新安全预检失败（HTTP ${response.status}）`);
      updatePreflight = (await response.json()) as UpdatePreflight;
    } catch (error) {
      updatePreflight = null;
      updateMessage = error instanceof Error ? error.message : '无法读取更新安全预检';
    }
  }

  async function prepareUpdate() {
    if (!updateStatus || !updateBundleRoot.trim()) {
      updateMessage = '请先填写离线安装包目录或 ZIP/TAR 文件';
      return;
    }
    updateLoading = true;
    updateMessage = '';
    try {
      const response = await apiFetch('/api/v1/update/prepare', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ bundle_root: updateBundleRoot.trim(), allow_downgrade: false })
      });
      const body = (await response.json()) as { plan?: UpdatePlan; error?: string };
      if (!response.ok || !body.plan) throw new Error(body.error || `更新预检失败（HTTP ${response.status}）`);
      updatePlan = body.plan;
      updateMessage = `已生成 ${body.plan.version} 版本计划，请审阅后确认切换。`;
      await loadUpdateStatus();
    } catch (error) {
      updateMessage = error instanceof Error ? error.message : '更新预检失败';
    } finally {
      updateLoading = false;
    }
  }

  async function activateUpdate() {
    if (!updatePlan || updateLoading || !window.confirm(`确认将下一次启动版本切换为 ${updatePlan.version}？当前 daemon 不会自动重启。`)) return;
    updateLoading = true;
    updateMessage = '';
    try {
      const response = await apiFetch('/api/v1/update/activate', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ plan_id: updatePlan.id })
      });
      const body = (await response.json()) as { result?: { active_version?: string }; error?: string };
      if (!response.ok || !body.result) throw new Error(body.error || `更新切换失败（HTTP ${response.status}）`);
      updatePlan = null;
      updateMessage = `已切换到 ${body.result.active_version || '新版本'}；请按部署方式重启 daemon。`;
      await loadUpdateStatus();
      await loadUpdatePreflight();
    } catch (error) {
      updateMessage = error instanceof Error ? error.message : '更新切换失败';
    } finally {
      updateLoading = false;
    }
  }

  async function rollbackUpdate() {
    if (!updateStatus?.state.previous_version || updateLoading || !window.confirm(`确认回滚到 ${updateStatus.state.previous_version}？`)) return;
    updateLoading = true;
    updateMessage = '';
    try {
      const response = await apiFetch('/api/v1/update/rollback', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
      const body = (await response.json()) as { result?: { active_version?: string }; error?: string };
      if (!response.ok || !body.result) throw new Error(body.error || `更新回滚失败（HTTP ${response.status}）`);
      updateMessage = `已回滚到 ${body.result.active_version || '旧版本'}；请按部署方式重启 daemon。`;
      await loadUpdateStatus();
    } catch (error) {
      updateMessage = error instanceof Error ? error.message : '更新回滚失败';
    } finally {
      updateLoading = false;
    }
  }

  async function loadRecoveryItems() {
    const requestToken = ++recoveryRequestToken;
    const root = currentWorkspace?.root;
    recoveryLoading = true;
    try {
      const response = await apiFetch('/api/v1/operations/recovery');
      if (!response.ok) throw new Error(`读取待恢复操作失败（HTTP ${response.status}）`);
      const body = (await response.json()) as RecoveryResponse;
      if (requestToken !== recoveryRequestToken || root !== currentWorkspace?.root) return;
      recoveryAvailable = body.available ?? false;
      recoveryItems = body.items ?? [];
      recoveryErrors = body.errors ?? [];
      recoveryMessage = recoveryErrors.length ? recoveryErrors.join('；') : '';
    } catch (error) {
      if (requestToken === recoveryRequestToken && root === currentWorkspace?.root) recoveryMessage = error instanceof Error ? error.message : '无法读取待恢复操作';
    } finally {
      if (requestToken === recoveryRequestToken) recoveryLoading = false;
    }
  }

  async function recoverOperation(item: RecoveryItem) {
    if (recoveryExecuting || !window.confirm(`将按回收站回执恢复操作 ${item.operation_id}，并把 journal 标记为已回滚。继续？`)) return;
    recoveryExecuting = item.operation_id;
    recoveryMessage = '';
    try {
      const response = await apiFetch('/api/v1/operations/recover', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ operation_id: item.operation_id })
      });
      const body = (await response.json()) as { error?: string; blocked?: boolean; result?: { needs_review?: boolean; phase?: string } };
      if (!response.ok || body.blocked) throw new Error(body.error || `恢复未完成（HTTP ${response.status}）`);
      await loadRecoveryItems();
      await loadInventory(true);
      recoveryMessage = body.result?.needs_review ? '恢复已停止并进入人工复核，请检查回收站和 journal。' : '操作已恢复，源目录和控制面已重新读取。';
    } catch (error) {
      recoveryMessage = error instanceof Error ? error.message : '恢复操作失败';
    } finally {
      recoveryExecuting = '';
    }
  }

  async function loadLocalArchives() {
    localArchivesLoading = true;
    localArchivesMessage = '';
    try {
      const response = await apiFetch('/api/v1/archives/local');
      if (!response.ok) throw new Error(`读取本地归档清单失败（HTTP ${response.status}）`);
      const body = await response.json() as { available: boolean; items?: LocalArchiveItem[]; errors?: string[] };
      localArchivesAvailable = body.available ?? false;
      localArchives = body.items ?? [];
      if (body.errors?.length) localArchivesMessage = body.errors.join('；');
    } catch (error) {
      localArchivesAvailable = false;
      localArchives = [];
      localArchivesMessage = error instanceof Error ? error.message : '无法读取本地归档清单';
    } finally {
      localArchivesLoading = false;
    }
  }

  async function refreshLocalRestoreSources() {
    await Promise.all([loadLocalArchives(), loadLocalRestoreRecoveryItems()]);
  }

  async function previewLocalArchiveRestore() {
    if (localRestoreLoading || !localRestoreArchivePath.trim()) return;
    localRestoreLoading = true;
    localRestoreMessage = '';
    try {
      const response = await apiFetch('/api/v1/archives/local/restore/preview', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ archive_path: localRestoreArchivePath.trim(), target_root: localRestoreTargetRoot.trim(), source_operation_id: localRestoreSourceOperationID.trim(), archive_password: localRestorePassword })
      });
      const body = (await response.json()) as { plan?: LocalRestorePlan; error?: string; conflicts?: string[] };
      if (!response.ok || !body.plan) throw new Error(body.error || (body.conflicts || []).join('；') || `恢复预览失败（HTTP ${response.status}）`);
      localRestorePlan = body.plan;
      localRestoreMessage = body.plan.preview.ready ? `预览通过：${body.plan.preview.files.length} 个文件待恢复${body.plan.preview.encrypted ? '，归档已加密' : ''}。` : `目标冲突：${(body.plan.preview.conflicts || []).join('；')}`;
    } catch (error) {
      localRestorePlan = null;
      localRestoreMessage = error instanceof Error ? error.message : '恢复预览失败';
    } finally {
      localRestoreLoading = false;
    }
  }

  async function executeLocalArchiveRestore() {
    if (!localRestorePlan || localRestoreLoading || !window.confirm(`确认恢复归档 ${localRestorePlan.archive_path}？目标中的既有文件不会被覆盖。`)) return;
    localRestoreLoading = true;
    localRestoreMessage = '';
    try {
      const response = await apiFetch('/api/v1/archives/local/restore/execute', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ restore_operation_id: localRestorePlan.restore_operation_id, archive_password: localRestorePassword })
      });
      const body = (await response.json()) as { result?: { phase?: string; needs_review?: boolean }; error?: string };
      if (!response.ok || body.result?.needs_review) throw new Error(body.error || '恢复已进入人工复核');
      localRestoreMessage = `恢复完成：${body.result?.phase || 'committed'}。`;
      localRestorePlan = null;
      localRestorePassword = '';
      await Promise.all([loadInventory(true), loadLocalRestoreRecoveryItems()]);
    } catch (error) {
      localRestoreMessage = error instanceof Error ? error.message : '恢复执行失败';
      await loadLocalRestoreRecoveryItems();
    } finally {
      localRestoreLoading = false;
    }
  }

  async function loadLocalRestoreRecoveryItems() {
    localRestoreRecoveryLoading = true;
    try {
      const response = await apiFetch('/api/v1/archives/local/restore/recovery');
      if (!response.ok) throw new Error(`读取归档恢复 journal 失败（HTTP ${response.status}）`);
      const body = (await response.json()) as LocalRestoreRecoveryResponse;
      localRestoreRecoveryAvailable = body.available ?? false;
      localRestoreRecoveryItems = body.items ?? [];
      if (body.errors?.length) localRestoreMessage = body.errors.join('；');
    } catch (error) {
      localRestoreRecoveryAvailable = false;
      localRestoreRecoveryItems = [];
      localRestoreMessage = error instanceof Error ? error.message : '无法读取归档恢复 journal';
    } finally {
      localRestoreRecoveryLoading = false;
    }
  }

  async function recoverLocalArchive(item: LocalRestoreRecoveryItem) {
    if (localRestoreRecoveryExecuting) return;
    localRestoreRecoveryExecuting = item.restore_operation_id;
    localRestoreMessage = '';
    try {
      const response = await apiFetch('/api/v1/archives/local/restore/recovery', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ restore_operation_id: item.restore_operation_id, archive_password: localRestorePassword })
      });
      const body = (await response.json()) as { result?: { needs_review?: boolean; phase?: string }; error?: string };
      if (!response.ok || body.result?.needs_review) throw new Error(body.error || '归档恢复仍需人工复核');
      localRestoreMessage = `归档恢复已续接：${body.result?.phase || 'committed'}。`;
      localRestorePassword = '';
      await Promise.all([loadInventory(true), loadLocalRestoreRecoveryItems()]);
    } catch (error) {
      localRestoreMessage = error instanceof Error ? error.message : '归档恢复续接失败';
      await loadLocalRestoreRecoveryItems();
    } finally {
      localRestoreRecoveryExecuting = '';
    }
  }

  function resumeCopyPlan(task: CopyTask) {
    copyTask = task;
    copyDestination = task.destination_root ?? task.plan?.destination_root ?? '';
    copyMessage = '已恢复落盘计划。请再次确认摘要后制作副本。';
  }

  async function selectWorkspace(id: string, refresh = false) {
    if (!id || (!refresh && id === currentWorkspace?.id)) return;
    workspaceLoading = true;
    workspaceMessage = '';
    try {
      const response = await apiFetch(`/api/v1/workspaces/${encodeURIComponent(id)}/select`, { method: 'POST' });
      const body = (await response.json()) as { error?: string };
      if (!response.ok) throw new Error(body.error || `切换工作区失败（HTTP ${response.status}）`);
      await loadWorkspaces();
      await loadWorkspaceDiscovery();
      cancelExecutionConfirmation();
      destinationOpen = false;
      destinationStatus = null;
      destinationMessage = '';
      executionTasksRequestToken += 1;
      if (executionPollTimer) clearTimeout(executionPollTimer);
      executionPolling = false;
      executionBusy = false;
      executionTasks = [];
      executionTask = null;
      executionResult = null;
      submittedExecutionID = '';
      executionMessage = '';
      reconciledExecutionID = '';
      operationExecuting = false;
      batchOperationExecuting = false;
      pendingConversationRequestToken += 1;
      pendingOperationsRequestToken += 1;
      recoveryRequestToken += 1;
      pendingConversationItems = [];
      pendingConversationTotal = 0;
      conversationSnapshotAt = '';
      pendingConversationSnapshotAt = '';
      pendingOperations = [];
      recoveryItems = [];
      recoveryAvailable = false;
      recoveryMessage = '';
      pendingSelectedID = '';
      pendingDrawerOpen = false;
      pendingMessage = '';
      pendingConfirmationCode = '';
      pendingArchivePassword = '';
      pendingPreviewCode = '';
      pendingReviewed = false;
      await loadInventory();
      await loadRecoveryItems();
      await loadCopyPlans();
      await loadPendingOperations();
      await loadRemoteCapabilities();
      await loadExecutionTasks();
    } catch (error) {
      workspaceMessage = error instanceof Error ? error.message : '无法切换工作区';
    } finally {
      workspaceLoading = false;
    }
  }

  async function addWorkspace(candidate?: CodexHomeCandidate) {
    if (workspacePersisted !== true) return;
    const root = candidate?.root ?? workspaceRoot.trim();
    if (!root) {
      workspaceMessage = '请输入工作区源根目录';
      return;
    }
    workspaceLoading = true;
    workspaceMessage = '';
    try {
      const response = await apiFetch('/api/v1/workspaces', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: candidate?.name ?? workspaceName.trim(), root, archive_db: candidate ? '' : workspaceArchiveDB.trim() })
      });
      const body = (await response.json()) as { error?: string; workspace?: WorkspaceDefinition };
      if (!response.ok) throw new Error(body.error || `添加工作区失败（HTTP ${response.status}）`);
      workspaceName = '';
      workspaceRoot = '';
      workspaceArchiveDB = '';
      if (body.workspace) await selectWorkspace(body.workspace.id, true);
    } catch (error) {
      workspaceMessage = error instanceof Error ? error.message : '无法添加工作区';
    } finally {
      workspaceLoading = false;
    }
  }

  async function removeWorkspace(id: string) {
    if (!id || id === currentWorkspace?.id || !window.confirm('只移除注册表中的工作区，不删除源目录。继续？')) return;
    workspaceLoading = true;
    workspaceMessage = '';
    try {
      const response = await apiFetch(`/api/v1/workspaces/${encodeURIComponent(id)}/remove`, { method: 'DELETE' });
      const body = (await response.json()) as { error?: string };
      if (!response.ok) throw new Error(body.error || `移除工作区失败（HTTP ${response.status}）`);
      await loadWorkspaces();
      await loadWorkspaceDiscovery();
    } catch (error) {
      workspaceMessage = error instanceof Error ? error.message : '无法移除工作区';
    } finally {
      workspaceLoading = false;
    }
  }

  async function loadGitHubArchives() {
    githubArchivesLoading = true;
    githubArchiveMessage = '';
    try {
      const response = await apiFetch('/api/v1/archives');
      if (!response.ok) throw new Error(`读取远端归档列表失败（HTTP ${response.status}）`);
      const body = (await response.json()) as { configured: boolean; provider_configured?: boolean; provider?: RemoteArchiveProviderInfo; target_required?: boolean; credential_available?: boolean; archives: GitHubArchiveSummary[] };
      remoteProvider = body.provider ?? remoteProvider;
      remoteProviderConfigured = body.provider_configured ?? body.configured;
      remoteTargetRequired = body.target_required ?? remoteProvider?.target_required ?? false;
      githubArchivesConfigured = body.configured;
      githubArchivesCredentialAvailable = body.credential_available ?? body.configured;
      githubArchives = body.archives ?? [];
    } catch (error) {
      githubArchives = [];
      githubArchiveMessage = error instanceof Error ? error.message : '无法读取远端归档列表';
    } finally {
      githubArchivesLoading = false;
    }
  }

  async function verifyGitHubArchive(archiveID: string) {
    githubArchiveVerifying = archiveID;
    githubArchiveMessage = '';
    try {
      const response = await apiFetch(`/api/v1/archives/${encodeURIComponent(archiveID)}/verify`);
      if (!response.ok) throw new Error(`远端归档校验失败（HTTP ${response.status}）`);
      githubArchiveMessage = `${archiveID} 已通过远端清单和分片校验`;
    } catch (error) {
      githubArchiveMessage = error instanceof Error ? error.message : '无法校验远端归档';
    } finally {
      githubArchiveVerifying = '';
    }
  }

  async function previewGitHubArchiveDownload(archiveID: string) {
    githubArchiveDownloading = archiveID;
    githubArchiveMessage = '';
    githubDownloadPlan = null;
    try {
      const response = await apiFetch(`/api/v1/archives/${encodeURIComponent(archiveID)}/download/preview`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ destination: githubDownloadDestination })
      });
      const body = await response.json() as GitHubDownloadPlan & { error?: string };
      if (!response.ok) throw new Error(body.error || `生成下载预览失败（HTTP ${response.status}）`);
      githubDownloadPlan = body;
    } catch (error) {
      githubArchiveMessage = error instanceof Error ? error.message : '无法生成远端归档下载预览';
    } finally {
      githubArchiveDownloading = '';
    }
  }

  async function executeGitHubArchiveDownload() {
    if (!githubDownloadPlan) return;
    githubArchiveDownloading = githubDownloadPlan.archive_id;
    githubArchiveMessage = '';
    try {
      const response = await apiFetch(`/api/v1/archives/${encodeURIComponent(githubDownloadPlan.archive_id)}/download/execute`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ operation_id: githubDownloadPlan.operation_id })
      });
      const body = await response.json() as { error?: string; output_directory?: string };
      if (!response.ok) throw new Error(body.error || `下载归档失败（HTTP ${response.status}）`);
      githubArchiveMessage = `远端归档已下载到 ${body.output_directory || githubDownloadPlan.output_directory}`;
      githubDownloadPlan = null;
    } catch (error) {
      githubArchiveMessage = error instanceof Error ? error.message : '无法下载远端归档';
    } finally {
      githubArchiveDownloading = '';
    }
  }

  async function previewRemoteArchiveRestore(archiveID: string) {
    if (!remoteRestoreTargetRoot.trim()) {
      githubArchiveMessage = '请先填写本机恢复目标目录';
      return;
    }
    githubArchiveRestoring = archiveID;
    githubArchiveMessage = '';
    remoteRestorePlan = null;
    try {
      const response = await apiFetch(`/api/v1/archives/${encodeURIComponent(archiveID)}/restore/preview`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ target_root: remoteRestoreTargetRoot.trim(), source_operation_id: remoteRestoreSourceOperationID.trim(), archive_password: remoteRestorePassword })
      });
      const body = await response.json() as RemoteRestorePlan & { error?: string };
      if (!response.ok || !body.plan) throw new Error(body.error || `生成远端恢复预览失败（HTTP ${response.status}）`);
      remoteRestorePlan = body;
      githubArchiveMessage = body.plan.preview.ready ? `远端归档已下载并通过恢复预览：${body.plan.preview.files.length} 个文件待恢复。` : `远端恢复存在目标冲突：${(body.plan.preview.conflicts || []).join('；')}`;
    } catch (error) {
      githubArchiveMessage = error instanceof Error ? error.message : '无法生成远端恢复预览';
    } finally {
      githubArchiveRestoring = '';
    }
  }

  async function executeRemoteArchiveRestore() {
    if (!remoteRestorePlan || githubArchiveRestoring || !window.confirm(`确认恢复远端归档 ${remoteRestorePlan.archive_id}？目标中的既有文件不会被覆盖。`)) return;
    githubArchiveRestoring = remoteRestorePlan.archive_id;
    githubArchiveMessage = '';
    try {
      const response = await apiFetch(`/api/v1/archives/${encodeURIComponent(remoteRestorePlan.archive_id)}/restore/execute`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ operation_id: remoteRestorePlan.restore_operation_id, archive_password: remoteRestorePassword })
      });
      const body = await response.json() as { result?: { phase?: string; needs_review?: boolean }; error?: string };
      if (!response.ok || body.result?.needs_review) throw new Error(body.error || '远端恢复已进入人工复核');
      githubArchiveMessage = `远端归档恢复完成：${body.result?.phase || 'committed'}。本地 staging 归档仍保留，可用于复核。`;
      remoteRestorePlan = null;
      remoteRestorePassword = '';
      await loadLocalRestoreRecoveryItems();
    } catch (error) {
      githubArchiveMessage = error instanceof Error ? error.message : '远端归档恢复失败';
      await loadLocalRestoreRecoveryItems();
    } finally {
      githubArchiveRestoring = '';
    }
  }

  async function previewGitHubArchiveDelete(archiveID: string) {
    githubArchiveDeleting = archiveID;
    githubArchiveMessage = '';
    githubDeletePlan = null;
    githubDeleteConfirmation = '';
    try {
      const response = await apiFetch(`/api/v1/archives/${encodeURIComponent(archiveID)}/delete/preview`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}'
      });
      const body = await response.json() as GitHubDeletePlan & { error?: string };
      if (!response.ok) throw new Error(body.error || `生成删除预览失败（HTTP ${response.status}）`);
      githubDeletePlan = body;
    } catch (error) {
      githubArchiveMessage = error instanceof Error ? error.message : '无法生成远端归档删除预览';
    } finally {
      githubArchiveDeleting = '';
    }
  }

  async function executeGitHubArchiveDelete() {
    if (!githubDeletePlan || githubDeleteConfirmation !== githubDeletePlan.confirmation_required) return;
    githubArchiveDeleting = githubDeletePlan.archive_id;
    githubArchiveMessage = '';
    try {
      const response = await apiFetch(`/api/v1/archives/${encodeURIComponent(githubDeletePlan.archive_id)}/delete/execute`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ operation_id: githubDeletePlan.operation_id, confirm_archive_id: githubDeleteConfirmation })
      });
      const body = await response.json() as { error?: string };
      if (!response.ok) throw new Error(body.error || `删除远端归档失败（HTTP ${response.status}）`);
      githubArchiveMessage = `远端归档 ${githubDeletePlan.archive_id} 已删除`;
      githubDeletePlan = null;
      githubDeleteConfirmation = '';
      await loadGitHubArchives();
    } catch (error) {
      githubArchiveMessage = error instanceof Error ? error.message : '无法删除远端归档';
    } finally {
      githubArchiveDeleting = '';
    }
  }

  async function loadGitHubRepositories() {
    githubRepositoriesLoading = true;
    githubRepositoryMessage = '';
    try {
      const response = await apiFetch('/api/v1/github/repositories');
      if (!response.ok) { const body = await response.json(); throw new Error(body.error || `读取 GitHub 仓库失败（HTTP ${response.status}）`); }
      const body = (await response.json()) as { configured: boolean; credential_available?: boolean; repositories: GitHubRepositorySummary[] };
      githubRepositoriesConfigured = body.configured;
      githubRepositoriesCredentialAvailable = body.credential_available ?? body.configured;
      githubRepositories = body.repositories ?? [];
    } catch (error) {
      githubRepositories = [];
      githubRepositoriesCredentialAvailable = false;
      githubRepositoryMessage = error instanceof Error ? error.message : '无法读取 GitHub 仓库';
    } finally {
      githubRepositoriesLoading = false;
    }
  }

  async function loadGitHubArchiveTarget() {
    githubArchiveTargetLoading = true;
    try {
      const response = await apiFetch('/api/v1/github/archive-target');
      if (!response.ok) throw new Error(`读取归档仓库选择失败（HTTP ${response.status}）`);
      const body = (await response.json()) as { configured: boolean; repository?: GitHubRepositorySummary };
      githubArchiveTarget = body.configured ? (body.repository ?? null) : null;
    } catch (error) {
      githubRepositoryMessage = error instanceof Error ? error.message : '无法读取归档仓库选择';
    } finally {
      githubArchiveTargetLoading = false;
    }
  }

  async function selectGitHubArchiveTarget(repository: GitHubRepositorySummary) {
    githubArchiveTargetSaving = true;
    githubRepositoryMessage = '';
    try {
      const response = await apiFetch('/api/v1/github/archive-target', {
        method: 'PUT', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ owner: repository.owner, repository: repository.name })
      });
      if (!response.ok) { const body = await response.json(); throw new Error(body.error || `选择归档仓库失败（HTTP ${response.status}）`); }
      githubDownloadPlan = null;
      githubDeletePlan = null;
      remoteRestorePlan = null;
      githubArchiveTarget = repository;
      githubRepositoryMessage = `已选择 ${repository.full_name} 作为归档仓库`;
      await loadGitHubArchives();
      return true;
    } catch (error) {
      githubRepositoryMessage = error instanceof Error ? error.message : '无法选择归档仓库';
      return false;
    } finally {
      githubArchiveTargetSaving = false;
    }
  }

  async function createGitHubRepository() {
    githubRepositoryCreating = true;
    githubRepositoryMessage = '';
    try {
      let repository = githubCreatedRepository ?? githubRepositories.find(item => item.name === githubRepositoryName.trim() && item.owner === githubAccount?.login);
      if (!repository) {
        const response = await apiFetch('/api/v1/github/repositories', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name: githubRepositoryName, description: githubRepositoryDescription })
        });
        const body = await response.json() as { error?: string; repository?: GitHubRepositorySummary };
        if (!response.ok) throw new Error(body.error || `创建私有仓库失败（HTTP ${response.status}）`);
        if (!body.repository?.private) throw new Error('未返回可用的私有仓库；请刷新仓库列表检查创建结果');
        repository = body.repository;
        githubCreatedRepository = repository;
      }
      if (!repository.private) throw new Error('同名仓库是公开仓库，请使用其他名称');
      if (await selectGitHubArchiveTarget(repository)) {
        githubCreatedRepository = null;
        await loadGitHubRepositories();
        githubRepositoryMessage = `已使用 ${repository.full_name}，下次启动会恢复此选择`;
      } else if (githubCreatedRepository) {
        githubRepositoryMessage = `私有仓库 ${repository.full_name} 已创建，但未能设为归档目标。${githubRepositoryMessage}。点击“重试使用已创建仓库”即可继续。`;
      }
    } catch (error) {
      githubRepositoryMessage = error instanceof Error ? error.message : '无法创建 GitHub 私有仓库';
    } finally {
      githubRepositoryCreating = false;
    }
  }

  async function loadInventory(preserveOperationMessage = false, autoRefresh = true) {
    loading = true;
    errorMessage = '';
    try {
      const response = await apiFetch('/api/v1/inventory?offset=0&limit=1');
      if (!response.ok) throw new Error(`读取 inventory 失败（HTTP ${response.status}）`);
      const inventory = (await response.json()) as InventoryReport;
      report = { ...inventory, conversations: report?.root === inventory.root ? report.conversations : [] };
      if (autoRefresh && report.stale) {
        await refreshInventory(preserveOperationMessage);
        return;
      }
      conversationTotal = inventory.conversation_total ?? inventory.conversations.length;
      conversationOffset = 0;
      selectedConversation = null;
      conversationSummary = null;
      projectRelation = null;
      projectMessage = '';
      projectLoading = false;
      summaryResults = {};
      summaryTask = null;
      summaryMessage = '';
      summaryPollToken += 1;
      summaryBatchLoading = false;
      selectedAssets = [];
      selectedAsset = null;
      impactPreview = null;
      operationConfirmationCode = '';
      operationArchivePassword = '';
      if (!preserveOperationMessage) operationMessage = '';
      restoreConversationSelection();
      batchOperationPreview = null;
      if (!preserveOperationMessage) batchOperationMessage = '';
      resetAssetGraphState();
      listScrollTop = 0;
      await loadConversations(true);
      void loadLatestBatchReview();
      inventoryReady = true;
      if (activeWorkspace === 'pending') void loadPendingConversations();
      if (search.trim()) await runSearch(false, false);
    } catch (error) {
      errorMessage = error instanceof Error ? error.message : '无法连接本地 daemon';
    } finally {
      loading = false;
    }
  }

  async function loadPendingOperations() {
    const requestToken = ++pendingOperationsRequestToken;
    const root = currentWorkspace?.root;
    pendingLoading = true;
    pendingLoadError = '';
    try {
      const response = await apiFetch('/api/v1/operations/pending');
      const body = await response.json() as { items?: Array<{ operation?: PendingOperation }>; error?: string };
      if (!response.ok) throw new Error(body.error || `读取暂存操作失败（HTTP ${response.status}）`);
      if (requestToken !== pendingOperationsRequestToken || root !== currentWorkspace?.root) return;
      pendingOperations = (body.items ?? []).map((item) => item.operation).filter((operation): operation is PendingOperation => Boolean(operation));
    } catch (error) {
      if (requestToken === pendingOperationsRequestToken && root === currentWorkspace?.root) pendingLoadError = error instanceof Error ? error.message : '无法读取暂存操作';
    } finally {
      if (requestToken === pendingOperationsRequestToken) pendingLoading = false;
    }
  }

  function pendingConversation(operation: PendingOperation) {
    return pendingConversationItems.find((conversation) => conversation.id === operation.conversation_id) || conversations.find((conversation) => conversation.id === operation.conversation_id);
  }

  function pendingMatchesQuery(values: Array<string | undefined>) {
    return !pendingQuery || values.filter(Boolean).join('\n').toLocaleLowerCase().includes(pendingQuery.toLocaleLowerCase());
  }

  function pendingExpired(operation: PendingOperation) {
    return Boolean(operation.expires_at && new Date(operation.expires_at).getTime() < Date.now());
  }

  function pendingNextStep(operation: PendingOperation) {
    if (['completed', 'cancelled'].includes(operation.state)) return '已结束，仅保留记录';
    if (pendingExpired(operation)) return '意图已过期，请重新检查会话后创建';
    if (operation.state === 'waiting_for_idle') return '等待活动写入结束，再手动探测';
    if (operation.state === 'idle_detected') return '可重新生成影响预览';
    if (operation.state === 'confirmation_required') return '检查最新预览后，再次确认';
    if (operation.state === 'ready_for_fixture_execution') return '已确认，仅限测试副本；尚未执行';
    if (operation.state === 'revalidation_failed') return '资产发生变化，需要重新探测';
    return '查看阻塞原因与资产证据';
  }

  function pendingViewSearch() {
    const params = new URLSearchParams();
    if (pendingTab !== 'conversations') params.set('queue', pendingTab);
    if (pendingQuery) params.set('q', pendingQuery);
    if (pendingFilter) params.set('state', pendingFilter);
    const query = params.toString();
    return query ? `?${query}` : '';
  }

  function syncPendingLocation() {
    window.history.replaceState({}, '', `/pending${pendingViewSearch()}`);
  }

  function selectPendingTab(tab: typeof pendingTab) {
    pendingConversationRequestToken += 1;
    pendingConversationLoading = false;
    pendingTab = tab;
    pendingFilter = '';
    syncPendingLocation();
    if (tab === 'conversations') void loadPendingConversations();
  }

  function applyPendingFilters() {
    pendingQuery = pendingSearchDraft.trim();
    syncPendingLocation();
    if (pendingTab === 'conversations') {
      pendingConversationItems = [];
      pendingConversationTotal = 0;
      void loadPendingConversations();
    }
  }

  function clearPendingFilters() {
    pendingSearchDraft = '';
    pendingFilter = '';
    applyPendingFilters();
  }

  async function loadPendingConversations(more = false) {
    if (more && pendingConversationLoading) return;
    const requestToken = ++pendingConversationRequestToken;
    const root = currentWorkspace?.root;
    const offset = more ? pendingConversationItems.length : 0;
    const query = new URLSearchParams({ attention: 'true', offset: String(offset), limit: '50' });
    if (pendingTab === 'conversations' && pendingQuery) query.set('q', pendingQuery);
    if (pendingTab === 'conversations' && pendingFilter) query.set('status', pendingFilter);
    pendingConversationLoading = true;
    pendingConversationError = '';
    try {
      const response = await apiFetch(`/api/v1/conversations?${query}`);
      const body = await response.json() as { items?: ConversationSummary[]; total?: number; generated_at?: string; stale?: boolean; error?: string };
      if (!response.ok) throw new Error(body.error || `读取需关注会话失败（HTTP ${response.status}）`);
      if (requestToken !== pendingConversationRequestToken || root !== currentWorkspace?.root) return;
      pendingConversationItems = more ? [...pendingConversationItems, ...(body.items ?? [])] : body.items ?? [];
      pendingConversationTotal = body.total ?? pendingConversationItems.length;
      pendingConversationSnapshotAt = body.generated_at || '';
      pendingConversationStale = Boolean(body.stale);
      reconcileRemovedConversationRows();
    } catch (error) {
      if (requestToken === pendingConversationRequestToken && root === currentWorkspace?.root) pendingConversationError = error instanceof Error ? error.message : '无法读取需关注会话';
    } finally {
      if (requestToken === pendingConversationRequestToken) pendingConversationLoading = false;
    }
  }

  async function refreshPendingWorkspace() {
    await Promise.all([loadPendingConversations(), loadPendingOperations(), loadRecoveryItems()]);
  }

  function inspectPendingOperation(operation: PendingOperation) {
    pendingSelectedID = operation.operation_id;
    pendingDrawerOpen = true;
    pendingConfirmationCode = '';
    pendingArchivePassword = '';
    pendingPreviewCode = '';
    pendingReviewed = false;
    pendingMessage = '';
  }

  function openRecoverySettings() {
    navigateToRoute({ page: 'settings', category: 'archives' });
  }

  async function stageCurrentOperation() {
    if (!selectedConversation || pendingBusy) return;
    pendingBusy = selectedConversation.id;
    pendingMessage = '';
    try {
      const response = await apiFetch('/api/v1/operations/pending', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ conversation_id: selectedConversation.id, ...effectiveOperationOptions(), confirmation_code: operationConfirmationCode.trim() })
      });
      const body = await response.json() as PendingOperationResponse;
      if (!response.ok && !body.operation) throw new Error(body.error || `暂存操作失败（HTTP ${response.status}）`);
      pendingMessage = body.error || `已保存暂存意图：${pendingStateLabels[body.operation?.state || 'pending'] || body.operation?.state || '待处理'}。`;
      await loadPendingOperations();
      selectWorkspaceView('pending');
    } catch (error) {
      pendingMessage = error instanceof Error ? error.message : '无法保存暂存操作';
    } finally {
      pendingBusy = '';
    }
  }

  async function refreshPendingOperation(operation: PendingOperation) {
    if (pendingBusy) return;
    pendingBusy = operation.operation_id;
    pendingMessage = '';
    pendingReviewed = false;
    pendingPreviewCode = '';
    try {
      const response = await apiFetch(`/api/v1/operations/pending/${encodeURIComponent(operation.operation_id)}/refresh`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
      const body = await response.json() as PendingOperationResponse;
      if (!response.ok && !body.operation) throw new Error(body.error || `刷新暂存状态失败（HTTP ${response.status}）`);
      pendingMessage = body.error || `已重新探测：${pendingStateLabels[body.operation?.state || operation.state] || body.operation?.state || operation.state}。`;
      await loadPendingOperations();
    } catch (error) {
      pendingMessage = error instanceof Error ? error.message : '无法刷新暂存状态';
    } finally {
      pendingBusy = '';
    }
  }

  async function repreviewPendingOperation(operation: PendingOperation) {
    if (pendingBusy) return;
    if (!pendingDrawerOpen || pendingSelectedID !== operation.operation_id) inspectPendingOperation(operation);
    pendingBusy = operation.operation_id;
    pendingMessage = '';
    pendingReviewed = false;
    pendingConfirmationCode = '';
    pendingPreviewCode = operation.strategy.permanent_delete ? String(100000 + crypto.getRandomValues(new Uint32Array(1))[0] % 900000) : '';
    try {
      const response = await apiFetch(`/api/v1/operations/pending/${encodeURIComponent(operation.operation_id)}/preview`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ...operation.strategy, confirmation_code: pendingPreviewCode })
      });
      const body = await response.json() as PendingOperationResponse;
      if (!response.ok && !body.operation) throw new Error(body.error || `重新生成暂存预览失败（HTTP ${response.status}）`);
      pendingMessage = body.error || '已重新生成影响预览，请检查资产并重新确认。';
      await loadPendingOperations();
    } catch (error) {
      pendingMessage = error instanceof Error ? error.message : '无法重新生成暂存预览';
    } finally {
      pendingBusy = '';
    }
  }

  async function confirmPendingOperation(operation: PendingOperation) {
    if (pendingBusy || !pendingReviewed || !lifecycleWritesEnabled || !lifecycleFixtureOnly || pendingExpired(operation) || operation.state !== 'confirmation_required' || !operation.preview) return;
    if (operation.preview?.requires_confirmation && (!pendingPreviewCode || pendingConfirmationCode.trim() !== pendingPreviewCode)) return;
    pendingBusy = operation.operation_id;
    pendingMessage = '';
    try {
      const response = await apiFetch(`/api/v1/operations/pending/${encodeURIComponent(operation.operation_id)}/confirm`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ confirmation_code: pendingConfirmationCode.trim(), archive_password: pendingArchivePassword })
      });
      const body = await response.json() as PendingOperationResponse;
      if (!response.ok && !body.operation) throw new Error(body.error || `重新确认失败（HTTP ${response.status}）`);
      pendingMessage = body.error || (body.idempotent ? '该暂存操作已经确认，未重复执行。' : '已重新确认，等待进入副本执行阶段。');
      await loadPendingOperations();
      pendingArchivePassword = '';
      pendingConfirmationCode = '';
      pendingReviewed = false;
    } catch (error) {
      pendingMessage = error instanceof Error ? error.message : '无法重新确认暂存操作';
    } finally {
      pendingBusy = '';
    }
  }

  async function cancelPendingOperation(operation: PendingOperation) {
    if (pendingBusy || !window.confirm('取消这条暂存意图？只更新暂存记录，不会修改源对话。')) return;
    pendingBusy = operation.operation_id;
    pendingMessage = '';
    try {
      const response = await apiFetch(`/api/v1/operations/pending/${encodeURIComponent(operation.operation_id)}/cancel`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
      const body = await response.json() as PendingOperationResponse;
      if (!response.ok && !body.operation) throw new Error(body.error || `取消暂存操作失败（HTTP ${response.status}）`);
      pendingMessage = body.error || '暂存操作已取消，审核记录仍保留。';
      await loadPendingOperations();
    } catch (error) {
      pendingMessage = error instanceof Error ? error.message : '无法取消暂存操作';
    } finally {
      pendingBusy = '';
    }
  }

  async function refreshInventory(preserveOperationMessage = false) {
    loading = true;
    errorMessage = '';
    try {
      const response = await apiFetch('/api/v1/inventory/refresh', { method: 'POST' });
      const body = await response.json() as { task?: { id?: string; status?: string }; error?: string };
      if (!response.ok || !body.task?.id) throw new Error(body.error || `启动后台刷新失败（HTTP ${response.status}）`);
      const deadline = Date.now() + 10 * 60 * 1000;
      while (Date.now() < deadline) {
        const taskResponse = await apiFetch(`/api/v1/tasks/${encodeURIComponent(body.task.id)}`);
        const taskBody = await taskResponse.json() as { task?: { status?: string; error?: string }; error?: string };
        if (!taskResponse.ok || !taskBody.task) throw new Error(taskBody.error || '读取后台刷新任务失败');
        const status = taskBody.task.status;
        if (status === 'completed') break;
        if (status === 'failed' || status === 'cancelled') throw new Error(taskBody.task.error || `后台刷新${status === 'cancelled' ? '已取消' : '失败'}`);
        await new Promise((resolve) => setTimeout(resolve, 250));
      }
      if (Date.now() >= deadline) throw new Error('后台刷新超时，请稍后重新读取');
      await loadInventory(preserveOperationMessage, false);
    } catch (error) {
      errorMessage = error instanceof Error ? error.message : '后台刷新失败';
    } finally {
      loading = false;
    }
  }

  async function loadGitHubStatus() {
    if (githubStatusLoading) return;
    githubStatusLoading = true;
    if (githubPollTimer) clearTimeout(githubPollTimer);
    githubPollTimer = null;
    try {
      const response = await apiFetch('/api/v1/github/auth/status');
      if (!response.ok) {
        const body = await response.json();
        throw new Error(body.error || `读取 GitHub 绑定状态失败（HTTP ${response.status}）`);
      }
      const status = (await response.json()) as GitHubAuthStatus;
      const newlyBound = status.credential_available && !githubStatus?.credential_available;
      githubStatus = status;
      githubSession = status.configured ? (status.session ?? null) : null;
      githubMessage = githubSession?.status === 'authorized' && !status.credential_available
        ? 'GitHub 已完成授权，但本地凭证不可用，请重新连接。' : '';
      if (!status.credential_available) {
        githubAccount = null;
        githubAccountMessage = '';
      } else if (newlyBound || !githubAccount) {
        await loadGitHubAccount();
      }
      if (newlyBound && remoteProvider?.id === 'github') {
        await Promise.all([loadGitHubRepositories(), loadGitHubArchiveTarget(), loadGitHubArchives()]);
      }
    } catch (error) {
      githubMessage = error instanceof Error ? error.message : '无法读取 GitHub 绑定状态';
    } finally {
      githubStatusLoading = false;
      if (githubSession?.status === 'pending') startGitHubPolling();
    }
  }

  function startGitHubPolling() {
    if (githubPollTimer) clearTimeout(githubPollTimer);
    githubPollTimer = setTimeout(() => { githubPollTimer = null; void loadGitHubStatus(); }, 1000);
  }

  async function loadGitHubAccount() {
    if (githubAccountLoading) return;
    githubAccountLoading = true;
    githubAccountMessage = '';
    try {
      const response = await apiFetch('/api/v1/github/account');
      const body = await response.json();
      if (!response.ok) throw new Error(body.error || '账户核验失败');
      githubAccount = body.account;
    } catch (error) {
      githubAccount = null;
      githubAccountMessage = error instanceof Error ? error.message : '账户核验失败';
    } finally { githubAccountLoading = false; }
  }

  async function bindGitHubCLI() {
    githubLoading = true;
    githubMessage = '';
    try {
      const response = await apiFetch('/api/v1/github/auth/cli', { method: 'POST' });
      const body = await response.json();
      if (!response.ok) throw new Error(body.error || '无法使用 GitHub CLI 登录');
      githubSession = null;
      githubAccount = body.account;
      githubCreatedRepository = null;
      githubDownloadPlan = null;
      githubDeletePlan = null;
      remoteRestorePlan = null;
      await loadRemoteSetup();
    } catch (error) {
      githubMessage = error instanceof Error ? error.message : '无法使用 GitHub CLI 登录';
    } finally { githubLoading = false; }
  }

  async function copyGitHubCodeAndOpen() {
    if (!githubSession) return;
    const url = githubSession.verification_uri_complete || githubSession.verification_uri;
    const copying = navigator.clipboard?.writeText(githubSession.user_code);
    window.open(url, '_blank', 'noopener,noreferrer');
    try {
      if (!copying) throw new Error('浏览器不支持自动复制');
      await copying;
      githubCodeMessage = '授权码已复制，请在 GitHub 页面粘贴；授权完成后会自动连接。';
    } catch {
      githubCodeMessage = '浏览器未允许自动复制，请手动复制上方授权码。若未打开新页面，请点击“打开授权页面”。';
    }
  }

  async function startGitHubBinding() {
    githubLoading = true;
    githubMessage = '';
    githubCodeMessage = '';
    try {
      const response = await apiFetch('/api/v1/github/auth/start', { method: 'POST' });
      if (!response.ok) { const body = await response.json(); throw new Error(body.error || `启动 GitHub 设备绑定失败（HTTP ${response.status}）`); }
      const body = (await response.json()) as { session: GitHubAuthSession };
      githubSession = body.session;
      githubStatus = { ...githubStatus, configured: true, credential_available: githubStatus?.credential_available ?? false, session: githubSession };
      startGitHubPolling();
    } catch (error) {
      githubMessage = error instanceof Error ? error.message : '无法启动 GitHub 设备绑定';
    } finally {
      githubLoading = false;
    }
  }

  async function revokeGitHubBinding() {
    githubLoading = true;
    githubMessage = '';
    try {
      const response = await apiFetch('/api/v1/github/auth/revoke', { method: 'POST' });
      if (!response.ok) throw new Error(`解除 GitHub 绑定失败（HTTP ${response.status}）`);
      githubSession = null;
      githubAccount = null;
      githubCreatedRepository = null;
      githubDownloadPlan = null;
      githubDeletePlan = null;
      remoteRestorePlan = null;
      await loadGitHubStatus();
      await Promise.all([loadGitHubArchives(), loadGitHubRepositories()]);
    } catch (error) {
      githubMessage = error instanceof Error ? error.message : '无法解除 GitHub 绑定';
    } finally {
      githubLoading = false;
    }
  }

  function firstAsset(conversation: ConversationSummary | null, files: FileInventory[]) {
    const path = conversation?.rollout_paths[0];
    return path ? files.find((file) => file.relative_path === path) ?? null : null;
  }

  function resetAssetGraphState() {
    assetGraphLoading = false;
    assetGraphLoadingMore = false;
    assetGraphMessage = '';
    assetGraphNodeOffset = 0;
    assetGraphEdgeOffset = 0;
    assetGraphNodeTotal = 0;
    assetGraphEdgeTotal = 0;
    assetGraphHasMore = false;
  }

  async function chooseConversation(conversation: ConversationSummary) {
    detailView = 'reading';
    navigateToRoute({ page: 'session', conversationID: conversation.id }, false);
    await activateConversation(conversation);
  }

  function toggleFamily(id: string) {
    const next = new Set(expandedFamilyIDs);
    if (next.has(id)) next.delete(id); else next.add(id);
    expandedFamilyIDs = next;
  }

  function openRelatedConversation(id: string) {
    expandedFamilyIDs = new Set([...expandedFamilyIDs, id]);
    detailView = 'reading';
    navigateToRoute({ page: 'session', conversationID: id }, false);
    void restoreConversationFromRoute(id);
  }

  async function activateConversation(conversation: ConversationSummary, resolvedDetail?: ConversationDetail) {
    compactPane = 'detail';
    readerLocation = null;
    eventRequestToken += 1;
    events = [];
    eventTotal = 0;
    eventOffset = 0;
    eventsLoading = false;
    eventsLoadingMore = false;
    selectedConversation = conversation;
    errorMessage = '';
    routeConversationLoading = false;
    routeConversationError = '';
    conversationActivity = null;
    activityMessage = '';
    conversationSummary = summaryResults[conversation.id] ?? null;
    summaryExpanded = false;
    summaryMessage = '';
    projectRelation = null;
    projectMessage = '';
    projectLoading = false;
    selectedAssets = [];
    selectedAsset = null;
    invalidateOperationPreview();
    preview = '';
    previewStart = 0;
    previewLength = 0;
    operationConfirmationCode = '';
    operationArchivePassword = '';
    operationMessage = '';
    resetAssetGraphState();
    try {
      if (resolvedDetail) {
        selectedAssets = resolvedDetail.files ?? [];
        selectedConversation = resolvedDetail.conversation;
        selectedAsset = firstAsset(selectedConversation, selectedAssets);
        if (detailView === 'technical') showTechnicalDetails(false);
        if (detailView === 'technical') await loadEvents(true);
      } else {
        await Promise.all([loadConversationDetail(conversation.id), detailView === 'technical' ? loadEvents(true) : Promise.resolve()]);
      }
    } catch (error) {
      if (selectedConversation?.id === conversation.id) errorMessage = error instanceof Error ? error.message : '读取对话失败';
    }
  }

  async function restoreConversationFromRoute(conversationID: string) {
    const requestToken = ++routeRestoreToken;
    routeConversationLoading = true;
    routeConversationError = '';
    try {
      if (selectedConversation?.id === conversationID) return;
      const knownConversation = conversations.find((conversation) => conversation.id === conversationID);
      if (knownConversation) {
        await activateConversation(knownConversation);
        return;
      }

      selectedConversation = null;
      selectedAssets = [];
      selectedAsset = null;
      events = [];
      eventTotal = 0;
      const response = await apiFetch(`/api/v1/conversations/${encodeURIComponent(conversationID)}?graph=none&files=summary`);
      if (!response.ok) throw new Error(response.status === 404 ? '找不到这个对话，或当前工作区无法读取它。' : `读取对话失败（HTTP ${response.status}）`);
      const detail = (await response.json()) as ConversationDetail;
      if (requestToken !== routeRestoreToken || routeConversationID !== conversationID) return;
      await activateConversation(detail.conversation, detail);
    } catch (error) {
      if (requestToken === routeRestoreToken && routeConversationID === conversationID) {
        routeConversationError = error instanceof Error ? error.message : '无法读取这个对话';
      }
    } finally {
      if (requestToken === routeRestoreToken) routeConversationLoading = false;
    }
  }

  function showTechnicalDetails(updateURL = true) {
    detailView = 'technical';
    if (updateURL && selectedConversation) navigateToRoute({ page: 'session', conversationID: selectedConversation.id }, false);
    if (!events.length && selectedConversation) void loadEvents(true);
    if (!selectedConversation?.asset_graph) void loadAssetGraph(true);
    if (!conversationActivity && !activityLoading) void loadConversationActivity();
    if (!preview) void loadPreview();
  }

  function showReadingDetails() {
    detailView = 'reading';
    if (selectedConversation) navigateToRoute({ page: 'session', conversationID: selectedConversation.id }, false);
  }

  async function loadConversationActivity() {
    if (!selectedConversation) return;
    const conversationID = selectedConversation.id;
    activityLoading = true;
    activityMessage = '';
    try {
      const query = new URLSearchParams({ conversation_id: conversationID, interval_ms: '80', windows: '2' });
      const response = await apiFetch(`/api/v1/activity?${query}`);
      if (!response.ok) throw new Error(`活动探测失败（HTTP ${response.status}）`);
      const body = (await response.json()) as ActivityResponse;
      if (selectedConversation?.id !== conversationID) return;
      conversationActivity = body;
    } catch (error) {
      if (selectedConversation?.id === conversationID) activityMessage = error instanceof Error ? error.message : '活动探测失败';
    } finally {
      if (selectedConversation?.id === conversationID) activityLoading = false;
    }
  }

  function toggleConversationSelection(conversationID: string, checked: boolean) {
    executionResult = null;
    const next = new Set(selectedConversationIDs);
    if (checked) next.add(conversationID); else next.delete(conversationID);
    selectedConversationIDs = next;
    saveConversationSelection();
    invalidateBatchOperationPreview();
  }

  function selectLoadedConversations() {
    executionResult = null;
    selectedConversationIDs = new Set([...selectedConversationIDs, ...filteredConversations.map((conversation) => conversation.id)]);
    saveConversationSelection();
    invalidateBatchOperationPreview();
  }

  function clearConversationSelection() {
    executionResult = null;
    selectedConversationIDs = new Set();
    saveConversationSelection();
    invalidateBatchOperationPreview();
  }

  function restoreConversationSelection() {
    const root = currentWorkspace?.root || report?.root || '';
    if (!root || selectionRoot === root) return;
    selectionRoot = root;
    latestBatchReview = null;
    selectionCopyMessage = '';
    try {
      selectedConversationIDs = readConversationSelection(window.sessionStorage, root);
      selectionStorageError = false;
    } catch {
      selectedConversationIDs = new Set();
      selectionStorageError = true;
    }
    const completed = completedRemovalIDs(root, executionTasks);
    if (Array.from(selectedConversationIDs).some(id => completed.has(id))) {
      selectedConversationIDs = new Set(Array.from(selectedConversationIDs).filter(id => !completed.has(id)));
      saveConversationSelection();
    }
  }

  function saveConversationSelection() {
    selectionCopyMessage = '';
    try {
      writeConversationSelection(window.sessionStorage, selectionRoot, selectedConversationIDs);
      selectionStorageError = false;
    } catch {
      selectionStorageError = true;
    }
  }

  async function loadLatestBatchReview() {
    const root = selectionRoot;
    try {
      const response = await apiFetch('/api/v1/operations/batch/preview/latest');
      if (!response.ok) { if (selectionRoot === root) latestBatchReview = null; return; }
      const body = await response.json() as { read_only?: boolean; approval?: string; selection_origin: string; preview: BatchOperationPreview };
      if (selectionRoot === root && body.read_only && body.approval === 'required' && body.preview?.source_root === root && Array.isArray(body.preview.items)) {
        latestBatchReview = body;
        pruneLatestBatchSelection();
      }
    } catch { /* 最近预览恢复是补充入口；失败不影响当前选择。 */ }
  }

  function restoreLatestBatchSelection() {
    pruneLatestBatchSelection();
    if (!latestBatchReview || latestBatchReview.preview.source_root !== selectionRoot) return;
    executionResult = null;
    selectedConversationIDs = new Set(latestBatchReview.preview.items.map((item) => item.conversation_id));
    saveConversationSelection();
    invalidateBatchOperationPreview();
    batchPanelOpen = true;
    batchOperationMessage = latestBatchReview.selection_origin === 'reconstructed_from_report'
      ? '已按你提供的标题与 token 合计恢复候选选择，请逐项核对原始 ID，再重新生成影响预览。此草稿不授予写入权限。'
      : '已恢复最近预览的选择，请重新生成影响预览；旧计划不会用于执行。';
  }

  async function copySelectedConversationIDs() {
    if (selectionCopyLoading) return;
    const ids = Array.from(selectedConversationIDs);
    const root = selectionRoot;
    selectionCopyLoading = true;
    selectionCopyMessage = '';
    try {
      const sourceIDs: string[] = [];
      for (const id of ids) {
        let conversation = conversations.find((item) => item.id === id);
        if (!conversation) {
          const response = await apiFetch(`/api/v1/conversations/${encodeURIComponent(id)}`);
          const body = await response.json() as { conversation?: ConversationSummary; error?: string };
          if (!response.ok) throw new Error(body.error || `无法核对会话 ${id}`);
          conversation = body.conversation;
        }
        if (!conversation?.source_id) throw new Error(`会话 ${id} 缺少原始 ID，请先检查影响预览。`);
        sourceIDs.push(conversation.source_id);
      }
      if (selectionRoot !== root || ids.join() !== Array.from(selectedConversationIDs).join()) return;
      await navigator.clipboard.writeText(sourceIDs.join('\n'));
      selectionCopyMessage = `已复制 ${sourceIDs.length} 个原始 conversation ID；复制和选择均不授予写入权限。`;
    } catch (error) {
      if (selectionRoot === root) selectionCopyMessage = error instanceof Error ? error.message : '无法复制，请检查浏览器剪贴板权限。';
    } finally { selectionCopyLoading = false; }
  }

  function pruneLatestBatchSelection() {
    if (!latestBatchReview) return;
    if (executionTasks.some(task => task.source_root === selectionRoot && task.preview_id === latestBatchReview?.preview.preview_id)) {
      latestBatchReview = null;
      return;
    }
    const completed = completedRemovalIDs(selectionRoot, executionTasks);
    const items = latestBatchReview.preview.items.filter(item => !completed.has(item.conversation_id));
    latestBatchReview = items.length ? { ...latestBatchReview, preview: { ...latestBatchReview.preview, items } } : null;
  }

  function reconcileExecutionSelection() {
    const completed = completedRemovalIDs(selectionRoot, executionTasks);
    const remaining = new Set(Array.from(selectedConversationIDs).filter(id => !completed.has(id)));
    if (remaining.size !== selectedConversationIDs.size) {
      selectedConversationIDs = remaining;
      saveConversationSelection();
      invalidateBatchOperationPreview();
    }
    if (batchOperationPreview?.items.some(item => completed.has(item.conversation_id))) invalidateBatchOperationPreview();
    const removed = completedRemovalIDs(selectionRoot, executionTasks, conversationSnapshotAt || report?.generated_at);
    if (selectedConversation && removed.has(selectedConversation.id)) {
      selectedConversation = null;
      impactPreview = null;
      reviewPanelOpen = false;
      if (completed.has(routeConversationID)) {
        const keepBatchOpen = batchPanelOpen;
        navigateToRoute({ page: 'sessions' }, false, true);
        batchPanelOpen = keepBatchOpen;
      }
    }
    reconcileRemovedConversationRows();
    pruneLatestBatchSelection();
  }

  // 只移除服务端已确认提交且晚于列表快照的会话，不等待整批完成或目录重扫。
  function reconcileRemovedConversationRows() {
    const root = currentWorkspace?.root || '';
    const removed = completedRemovalIDs(root, executionTasks, conversationSnapshotAt || report?.generated_at);
    const removedPaths = new Set((report?.conversations || []).filter(item => removed.has(item.id)).flatMap(item => item.rollout_paths));
    if (report?.root === root) {
      const items = report.conversations.filter(item => !removed.has(item.id));
      const count = report.conversations.length - items.length;
      if (count) {
        conversationTotal = Math.max(0, conversationTotal - count);
        conversationOffset = Math.max(0, conversationOffset - count);
        report = { ...report, conversations: items, conversation_total: conversationTotal };
        summaryResults = Object.fromEntries(Object.entries(summaryResults).filter(([id]) => !removed.has(id)));
      }
    }
    searchHits = searchHits.filter(hit => !removed.has(hit.conversation_id || '') && !removedPaths.has(hit.relative_path));
    const pendingRemoved = completedRemovalIDs(root, executionTasks, pendingConversationSnapshotAt);
    const pendingItems = pendingConversationItems.filter(item => !pendingRemoved.has(item.id));
    pendingConversationTotal = Math.max(0, pendingConversationTotal - (pendingConversationItems.length - pendingItems.length));
    pendingConversationItems = pendingItems;
  }

  async function loadConversations(reset = true, preserveView = false) {
    if (!reset && (conversationsLoading || conversationsLoadingMore)) return;
    const requestToken = ++conversationRequestToken;
    const sourceRoot = currentWorkspace?.root;
    restoreConversationSelection();
    if (reset) {
      conversationsLoading = true;
      if (!preserveView) {
        invalidateOperationPreview();
        conversationOffset = 0;
        listScrollTop = 0;
        if (conversationListElement) conversationListElement.scrollTop = 0;
      }
    } else {
      conversationsLoadingMore = true;
    }
    try {
      const limit = preserveView ? Math.min(1000, Math.max(conversationPageSize, conversationOffset)) : conversationPageSize;
      const query = new URLSearchParams({ offset: String(reset ? 0 : conversationOffset), limit: String(limit) });
      if (statusFilter !== '全部状态') query.set('status', statusFilter);
      if (conversationQuery.trim()) query.set('q', conversationQuery.trim());
      if (cwdFilter.trim()) query.set('cwd', cwdFilter.trim());
      if (modelFilter.trim()) query.set('model', modelFilter.trim());
      if (modelFilter.trim() && modelExact) query.set('model_exact', 'true');
      if (cwdFilter.trim() && cwdExact) query.set('cwd_exact', 'true');
      if (fromFilter) query.set('from', fromFilter);
      if (toFilter) query.set('to', toFilter);
      if (minTokensFilter.trim()) query.set('min_tokens', minTokensFilter.trim());
      if (maxTokensFilter.trim()) query.set('max_tokens', maxTokensFilter.trim());
      if (minBytesFilter.trim()) query.set('min_bytes', minBytesFilter.trim());
      if (maxBytesFilter.trim()) query.set('max_bytes', maxBytesFilter.trim());
      if (diagnosticsFilter) query.set('has_diagnostics', diagnosticsFilter);
      const response = await apiFetch(`/api/v1/conversations?${query}`);
      if (!response.ok) throw new Error(`读取对话列表失败（HTTP ${response.status}）`);
      const body = (await response.json()) as { items?: ConversationSummary[]; total?: number; generated_at?: string };
      if (requestToken !== conversationRequestToken || sourceRoot !== currentWorkspace?.root) return;
      const items = body.items ?? [];
      conversationTotal = body.total ?? items.length;
      conversationOffset = reset ? items.length : conversationOffset + items.length;
      conversationSnapshotAt = body.generated_at || '';
      if (reset && !preserveView) {
        batchOperationPreview = null;
        batchOperationMessage = '';
      }
      if (report) report = { ...report, conversations: reset ? items : [...report.conversations, ...items], conversation_total: conversationTotal };
      reconcileRemovedConversationRows();
      if (preserveView) return;
      if (reset) {
        const loadedItems = report?.conversations || items;
        const requestedID = activeWorkspace === 'sessions' ? routeConversationID : '';
        routeConversationLoading = Boolean(requestedID);
        routeConversationError = '';
        selectedConversation = requestedID ? loadedItems.find((item) => item.id === requestedID) ?? null : loadedItems[0] ?? null;
        conversationSummary = selectedConversation ? summaryResults[selectedConversation.id] ?? null : null;
        summaryMessage = '';
        projectRelation = null;
        projectMessage = '';
        projectLoading = false;
        selectedAssets = [];
        selectedAsset = null;
        conversationActivity = null;
        activityMessage = '';
        preview = '';
        previewStart = 0;
        previewLength = 0;
        if (selectedConversation) {
          await Promise.all([loadConversationDetail(selectedConversation.id), detailView === 'technical' ? loadEvents(true) : Promise.resolve()]);
          routeConversationLoading = false;
        } else if (requestedID) {
          await restoreConversationFromRoute(requestedID);
        }
      }
    } catch (error) {
      if (requestToken === conversationRequestToken) errorMessage = error instanceof Error ? error.message : '读取对话列表失败';
    } finally {
      if (requestToken === conversationRequestToken) {
        conversationsLoading = false;
        conversationsLoadingMore = false;
      }
    }
  }

  async function loadMoreConversations() {
    if (conversationOffset >= conversationTotal || conversationsLoading || conversationsLoadingMore) return;
    await loadConversations(false);
  }

  async function loadConversationDetail(conversationID: string) {
    const response = await apiFetch(`/api/v1/conversations/${encodeURIComponent(conversationID)}?graph=none&files=summary`);
    if (!response.ok) throw new Error(`读取对话详情失败（HTTP ${response.status}）`);
    const detail = (await response.json()) as ConversationDetail;
    if (selectedConversation?.id !== conversationID) return;
    selectedAssets = detail.files ?? [];
    selectedConversation = detail.conversation;
    selectedAsset = firstAsset(selectedConversation, selectedAssets);
    if (detailView === 'technical') showTechnicalDetails(false);
  }

  async function loadAssetGraph(reset = true, conversationID = selectedConversation?.id) {
    if (!conversationID || (assetGraphLoading && reset) || (assetGraphLoadingMore && !reset)) return;
    if (reset) {
      assetGraphLoading = true;
      assetGraphNodeOffset = 0;
      assetGraphEdgeOffset = 0;
      assetGraphNodeTotal = 0;
      assetGraphEdgeTotal = 0;
      assetGraphHasMore = false;
    } else {
      assetGraphLoadingMore = true;
    }
    assetGraphMessage = '';
    try {
      const query = new URLSearchParams({
        node_offset: String(reset ? 0 : assetGraphNodeOffset),
        edge_offset: String(reset ? 0 : assetGraphEdgeOffset),
        limit: String(assetGraphPageSize)
      });
      const response = await apiFetch(`/api/v1/conversations/${encodeURIComponent(conversationID)}/asset-graph?${query}`);
      const body = (await response.json()) as AssetGraphPageResponse;
      if (!response.ok || !body.asset_graph) throw new Error(body.error || `读取资产图失败（HTTP ${response.status}）`);
      if (selectedConversation?.id !== conversationID) return;
      const current = reset ? null : selectedConversation.asset_graph;
      const incoming = body.asset_graph;
      const graph = current ? {
        ...incoming,
        nodes: [...current.nodes, ...incoming.nodes],
        edges: [...current.edges, ...incoming.edges]
      } : incoming;
      selectedConversation = { ...selectedConversation, asset_graph: graph };
      assetGraphNodeOffset = body.node_offset + incoming.nodes.length;
      assetGraphEdgeOffset = body.edge_offset + incoming.edges.length;
      assetGraphNodeTotal = body.node_total;
      assetGraphEdgeTotal = body.edge_total;
      assetGraphHasMore = body.has_more;
    } catch (error) {
      if (selectedConversation?.id === conversationID) assetGraphMessage = error instanceof Error ? error.message : '无法读取资产图';
    } finally {
      assetGraphLoading = false;
      assetGraphLoadingMore = false;
    }
  }

  async function loadProjectRelation() {
    if (!selectedConversation) return;
    const conversationID = selectedConversation.id;
    projectLoading = true;
    projectMessage = '';
    try {
      const response = await apiFetch(`/api/v1/conversations/${encodeURIComponent(conversationID)}/project`);
      const body = (await response.json()) as { project?: ProjectRelationReport; error?: string };
      if (!response.ok || !body.project) throw new Error(body.error || `扫描项目关系失败（HTTP ${response.status}）`);
      if (selectedConversation?.id !== conversationID) return;
      projectRelation = body.project;
    } catch (error) {
      if (selectedConversation?.id === conversationID) projectMessage = error instanceof Error ? error.message : '无法扫描项目关系';
    } finally {
      if (selectedConversation?.id === conversationID) projectLoading = false;
    }
  }

  async function loadConversationSummary() {
    if (!selectedConversation) return;
    const conversationID = selectedConversation.id;
    summaryLoading = true;
    summaryMessage = '';
    try {
      const response = await apiFetch(`/api/v1/conversations/${encodeURIComponent(conversationID)}/summary?provider=${encodeURIComponent(summaryProviderID)}`);
      const body = (await response.json()) as { result?: ConversationSummaryResult; error?: string };
      if (!response.ok || !body.result) throw new Error(body.error || `读取摘要失败（HTTP ${response.status}）`);
      if (selectedConversation?.id !== conversationID) return;
      conversationSummary = body.result;
      summaryResults = { ...summaryResults, [conversationID]: body.result };
    } catch (error) {
      summaryMessage = error instanceof Error ? error.message : '无法生成本地摘要';
    } finally {
      summaryLoading = false;
    }
  }

  async function requestBatchSummary() {
    const conversationIDs = Array.from(selectedConversationIDs);
    if (!conversationIDs.length) {
      summaryMessage = '请先选择至少一个对话';
      return;
    }
    summaryBatchLoading = true;
    summaryMessage = '';
    summaryTask = null;
    const token = ++summaryPollToken;
    try {
      const response = await apiFetch('/api/v1/summaries', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ conversation_ids: conversationIDs, provider_id: summaryProviderID, max_events: 8, max_chars: 1800 })
      });
      const body = (await response.json()) as { task?: SummaryTask; error?: string };
      if (!response.ok || !body.task) throw new Error(body.error || `批量摘要启动失败（HTTP ${response.status}）`);
      summaryTask = body.task;
      for (let attempt = 0; attempt < 240; attempt += 1) {
        if (token !== summaryPollToken) return;
        if (summaryTask.status === 'completed' || summaryTask.status === 'failed' || summaryTask.status === 'cancelled') break;
        await new Promise((resolve) => setTimeout(resolve, 250));
        const taskResponse = await apiFetch(`/api/v1/summaries/${encodeURIComponent(body.task.id)}`);
        const taskBody = (await taskResponse.json()) as { task?: SummaryTask; error?: string };
        if (!taskResponse.ok || !taskBody.task) throw new Error(taskBody.error || `读取摘要进度失败（HTTP ${taskResponse.status}）`);
        summaryTask = taskBody.task;
      }
      for (const result of summaryTask?.results ?? []) {
        summaryResults = { ...summaryResults, [result.conversation_id]: result };
      }
      if (selectedConversation && summaryResults[selectedConversation.id]) conversationSummary = summaryResults[selectedConversation.id];
      summaryMessage = summaryTask?.status === 'failed' ? summaryTask.error || '批量摘要任务失败，请查看下方失败原因。'
        : summaryTask?.status === 'cancelled' ? '批量摘要任务已取消。'
        : summaryTask?.failed ? `批量摘要完成，但有 ${summaryTask.failed} 项失败。` : '批量摘要已完成，结果仅保存在当前页面。';
    } catch (error) {
      summaryMessage = error instanceof Error ? error.message : '批量摘要失败';
    } finally {
      if (token === summaryPollToken) summaryBatchLoading = false;
    }
  }

  function summaryConfidenceLabel(value: string) {
    return value === 'high' ? '高' : value === 'medium' ? '中' : '低';
  }

  function summaryTaskStatusLabel(value: string) {
    return value === 'queued' ? '排队中' : value === 'running' ? '生成中' : value === 'completed' ? '已完成' : value === 'cancelled' ? '已取消' : '失败';
  }

  function summaryProviderLabel(value: string) {
    return value === 'openai-compatible' ? '云端兼容模型' : value === 'local-deterministic' ? '本地规则摘录' : value;
  }

  function projectStatusLabel(value: string) {
    return value === 'detected' ? '已检测' : value === 'truncated' ? '预算截断' : value === 'unavailable' ? '不可用' : value;
  }

  function projectConfidenceLabel(value: string) {
    return value === 'high' ? '高' : value === 'medium' ? '中' : value === 'low' ? '低' : value;
  }

  function projectLabel(path: string) {
    return path.replaceAll('\\', '/').split('/').filter(Boolean).slice(-2).join('/') || path;
  }

  async function setStatusFilter(value: string) {
    statusFilter = value;
    selectWorkspaceView('sessions');
    await reloadSessionResults();
  }

  async function applyConversationFilters() {
    filterDraft = { ...filterDraft, modelExact: '', cwdExact: '' };
    applyFilterSnapshot(filterDraft);
    selectWorkspaceView('sessions');
    await reloadSessionResults();
  }

  async function clearConversationFilters() {
    applyFilterSnapshot(emptySessionFilters());
    selectWorkspaceView('sessions');
    await reloadSessionResults();
  }

  async function clearSearch() {
    search = '';
    searchDraft = '';
    searchRequestToken += 1;
    searching = false;
    searchHits = [];
    searchLimited = false;
    searchExecutedQuery = '';
    selectWorkspaceView('sessions');
    await loadConversations(true);
  }

  async function submitSearch() {
    search = searchDraft.trim();
    selectWorkspaceView('sessions');
    errorMessage = '';
    await runSearch();
  }

  function onConversationScroll(event: Event) {
    const target = event.currentTarget as HTMLElement;
    listScrollTop = target.scrollTop;
    listViewportHeight = target.clientHeight || listViewportHeight;
    if (!searchIsApplied && target.scrollTop + target.clientHeight >= target.scrollHeight - conversationRowHeight * 3) {
      void loadMoreConversations();
    }
  }

  function setOperationOption(option: keyof OperationOptions, checked: boolean) {
    operationOptions = { ...operationOptions, [option]: checked };
    if (option === 'encrypted' && checked) operationOptions = { ...operationOptions, archive: true };
    if (option === 'archive' && !checked) operationOptions = { ...operationOptions, encrypted: false };
    if (option === 'archive' && checked) operationOptions = { ...operationOptions, archive_local: true, archive_remote: true };
    if (option === 'trash' && checked) operationOptions = { ...operationOptions, permanent_delete: false };
    if (option === 'permanent_delete' && checked) operationOptions = { ...operationOptions, trash: false };
    invalidateOperationPreview();
  }

  function effectiveOperationOptions(selectedOptions: OperationOptions = operationOptions): OperationOptions {
    return { ...selectedOptions };
  }

  function operationSummary(selectedOptions: OperationOptions) {
    const options = effectiveOperationOptions(selectedOptions);
    const labels = [];
    if (options.archive) labels.push(options.encrypted ? '加密归档' : '归档');
    if (options.trash) labels.push('移入回收站');
    if (options.permanent_delete) labels.push('删除');
    return labels.join(' + ') || '未选择操作';
  }

  function invalidateOperationPreview() {
    cancelExecutionConfirmation();
    impactPreviewRequestToken += 1;
    previewLoading = false;
    impactPreview = null;
    operationMessage = '';
    invalidateBatchOperationPreview();
  }

  function invalidateBatchOperationPreview() {
    cancelExecutionConfirmation();
    batchPreviewRequestToken += 1;
    if (!batchOperationExecuting) batchOperationLoading = false;
    batchOperationPreview = null;
    batchOperationMessage = '';
  }

  async function loadEvents(reset = true) {
    if (!selectedConversation) return;
    const conversationID = selectedConversation.id;
    const requestToken = ++eventRequestToken;
    if (reset) {
      events = [];
      eventTotal = 0;
      eventOffset = 0;
      eventsLoading = true;
    } else {
      eventsLoadingMore = true;
    }
    try {
      const response = await apiFetch(`/api/v1/conversations/${encodeURIComponent(conversationID)}/events?offset=${eventOffset}&limit=80`);
      if (!response.ok) throw new Error('事件读取失败');
      const body = (await response.json()) as { items?: ConversationEvent[]; total?: number };
      if (requestToken !== eventRequestToken || selectedConversation?.id !== conversationID) return;
      const items = body.items ?? [];
      events = reset ? items : [...events, ...items];
      eventTotal = body.total ?? events.length;
      eventOffset += items.length;
    } catch (error) {
      if (requestToken !== eventRequestToken || selectedConversation?.id !== conversationID) return;
      errorMessage = error instanceof Error ? error.message : '事件读取失败';
    } finally {
      if (requestToken === eventRequestToken) {
        eventsLoading = false;
        eventsLoadingMore = false;
      }
    }
  }

  async function loadMoreEvents() {
    if (eventsLoading || eventsLoadingMore || eventOffset >= eventTotal) return;
    await loadEvents(false);
  }

  async function chooseAsset(file: FileInventory) {
    selectedAsset = file;
    await loadPreview();
  }

  async function openSearchHit(hit: SearchHit) {
    readerLocation = { path: hit.relative_path, start: hit.start, query: search.trim() };
    detailView = 'reading';
    if (selectedConversation) navigateToRoute({ page: 'session', conversationID: selectedConversation.id }, false);
  }

  async function openReaderEvidence(event: ReaderEvent) {
    const id = selectedConversation?.id;
    const workspace = report?.root;
    let file = selectedAssets.find((item) => item.relative_path === event.relative_path) || report?.files.find((item) => item.relative_path === event.relative_path);
    if (!file) {
      try {
        const response = await apiFetch(`/api/v1/inventory?${new URLSearchParams({ q: event.relative_path, offset: '0', limit: '20' })}`);
        if (!response.ok) throw new Error('读取历史来源失败');
        const body = await response.json() as InventoryReport;
        file = body.files?.find((item) => item.relative_path === event.relative_path);
      } catch { errorMessage = '历史来源暂时不可读，请重新加载对话。'; return; }
    }
    if (id !== selectedConversation?.id || workspace !== report?.root) return;
    if (!file) { errorMessage = '历史来源已不在当前工作区，请重新加载对话。'; return; }
    detailView = 'technical';
    if (selectedConversation) navigateToRoute({ page: 'session', conversationID: selectedConversation.id }, false);
    selectedAsset = file;
    if (!events.length) void loadEvents(true);
    await loadPreview(event.start_offset, Math.min(8192, event.end_offset - event.start_offset));
  }

  async function loadPreview(start = 0, requestedLength?: number) {
    preview = '';
    previewStart = Math.max(0, start);
    if (!selectedAsset) {
      previewLength = 0;
      return;
    }
    const length = Math.min(Math.max(1, selectedAsset.size_bytes - previewStart), requestedLength ?? 8192);
    const assetPath = selectedAsset.relative_path;
    const conversationID = selectedConversation?.id;
    previewLength = length;
    try {
      const query = new URLSearchParams({ path: selectedAsset.relative_path, start: String(previewStart), length: String(length) });
      const response = await apiFetch(`/api/v1/source-range?${query}`);
      if (!response.ok) throw new Error('片段读取失败');
      const text = new TextDecoder().decode(await response.arrayBuffer());
      if (selectedConversation?.id === conversationID && selectedAsset?.relative_path === assetPath) preview = text;
    } catch {
      if (selectedConversation?.id === conversationID && selectedAsset?.relative_path === assetPath) preview = '当前文件片段不可读取，详情页保留诊断状态。';
    }
  }

  async function runSearch(showErrors = true, selectFirstMatch = true) {
    const requestToken = ++searchRequestToken;
    const sourceRoot = currentWorkspace?.root;
    const requestedAt = new Date().toISOString();
    searchLimited = false;
    if (!search.trim()) {
      searchHits = [];
      searchExecutedQuery = '';
      searching = false;
      return;
    }
    const queryText = search.trim();
    searching = true;
    searchExecutedQuery = '';
    try {
      const query = new URLSearchParams({ q: queryText, limit: '200' });
      const response = await apiFetch(`/api/v1/search?${query}`);
      if (!response.ok) throw new Error(`搜索失败（HTTP ${response.status}）`);
      const body = (await response.json()) as { hits: SearchHit[]; candidate_limit_reached?: boolean; snapshot_generated_at?: string };
      if (requestToken !== searchRequestToken || sourceRoot !== currentWorkspace?.root) return;
      const snapshotAt = Date.parse(body.snapshot_generated_at || '') > 0 ? body.snapshot_generated_at : requestedAt;
      const removed = completedRemovalIDs(sourceRoot || '', executionTasks, snapshotAt);
      searchHits = (body.hits ?? []).filter(hit => !removed.has(hit.conversation_id || ''));
      searchLimited = Boolean(body.candidate_limit_reached);
      searchExecutedQuery = queryText;
      listScrollTop = 0;
      if (conversationListElement) conversationListElement.scrollTop = 0;
      const firstHit = searchHits[0];
      const firstMatch = firstHit ? conversations.find((conversation) => firstHit.conversation_id === conversation.id || conversation.rollout_paths.includes(firstHit.relative_path)) : undefined;
      if (selectFirstMatch && firstMatch && selectedConversation?.id !== firstMatch.id) {
        await activateConversation(firstMatch);
        compactPane = 'sessions';
      }
    } catch (error) {
      if (requestToken !== searchRequestToken || sourceRoot !== currentWorkspace?.root) return;
      searchHits = [];
      searchExecutedQuery = queryText;
      if (showErrors) errorMessage = error instanceof Error ? error.message : '搜索失败';
    } finally {
      if (requestToken === searchRequestToken) searching = false;
    }
  }

  async function loadImpactPreview() {
    if (!selectedConversation || !operationChosen || previewLoading || operationExecuting) return;
    if (operationSelectionError) return;
    const conversationID = selectedConversation.id;
    const requestToken = ++impactPreviewRequestToken;
    reviewPanelOpen = true;
    previewLoading = true;
    impactPreview = null;
    operationMessage = '';
    try {
      const options = effectiveOperationOptions();
      const response = await apiFetch('/api/v1/operations/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ conversation_id: conversationID, confirmation_code: operationConfirmationCode.trim(), ...options })
      });
      const body = (await response.json()) as ImpactPreview;
      if (requestToken !== impactPreviewRequestToken || selectedConversation?.id !== conversationID) return;
      if (!response.ok && !body.blocked && !body.plan && !body.asset_graph) throw new Error(body.error || `影响检查失败（HTTP ${response.status}）`);
      impactPreview = body;
    } catch (error) {
      if (requestToken === impactPreviewRequestToken) operationMessage = error instanceof Error ? error.message : '无法读取操作影响';
    } finally {
      if (requestToken === impactPreviewRequestToken) {
        previewLoading = false;
        await tick();
        singleImpactPanel?.scrollIntoView({ block: 'start' });
      }
    }
  }

  async function executeOperation() { await beginExecutionConfirmation('single'); }

  async function loadBatchOperationPreview() {
    if (!operationChosen || batchOperationLoading || batchOperationExecuting) return;
    batchPanelOpen = true;
    if (operationSelectionError) return;
    const conversationIDs = Array.from(selectedConversationIDs);
    if (!conversationIDs.length) {
      batchOperationMessage = '请先选择至少一个对话';
      return;
    }
    const requestToken = ++batchPreviewRequestToken;
    batchOperationLoading = true;
    batchOperationMessage = '';
    batchOperationPreview = null;
    try {
      const options = effectiveOperationOptions();
      const response = await apiFetch('/api/v1/operations/batch/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ conversation_ids: conversationIDs, confirmation_code: operationConfirmationCode.trim(), ...options })
      });
      const body = (await response.json()) as Partial<BatchOperationPreview> & { error?: string };
      if (requestToken !== batchPreviewRequestToken) return;
      if (!body.summary || !Array.isArray(body.items)) throw new Error(body.error || `批量预览失败（HTTP ${response.status}）`);
      batchOperationPreview = body as BatchOperationPreview;
      void loadLatestBatchReview();
    } catch (error) {
      if (requestToken === batchPreviewRequestToken) batchOperationMessage = error instanceof Error ? error.message : '无法读取批量操作影响';
    } finally {
      if (requestToken === batchPreviewRequestToken) {
        batchOperationLoading = false;
        await tick();
        batchImpactPanel?.scrollIntoView({ block: 'start' });
      }
    }
  }

  async function executeBatchOperation() { await beginExecutionConfirmation('batch'); }

  function cancelExecutionConfirmation() {
    executionOpen = false;
    executionChallenge = null;
    executionPassword = '';
    executionPreviewID = '';
    executionPackagedTrash = false;
  }

  async function beginExecutionConfirmation(origin: 'single' | 'batch') {
    if (executionBusy || (origin === 'single' ? !operationExecutable : !batchOperationExecutable)) return;
    const previewID = origin === 'single' ? impactPreview?.preview_id : batchOperationPreview?.preview_id;
    if (!previewID || !await checkExecutionDestinations()) return;
    if (previewID !== (origin === 'single' ? impactPreview?.preview_id : batchOperationPreview?.preview_id)) return;
    executionOrigin = origin;
    executionPreviewID = previewID;
    executionStrategy = { ...effectiveOperationOptions() };
    executionPackagedTrash = Boolean((origin === 'single' ? impactPreview : batchOperationPreview)?.restricted_preview?.trash_package);
    executionPassword = operationArchivePassword;
    executionSourceRoot = (origin === 'single' ? impactPreview?.source_root : batchOperationPreview?.source_root) || currentWorkspace?.root || '';
    const previewItems = origin === 'batch' ? batchOperationPreview?.items : impactPreview?.items;
    executionTargets = previewItems?.length
      ? reviewFamilyItems(previewItems).map(item => ({ id: item.source_id || item.conversation_id, title: item.title || '', automaticallyIncluded: item.automatically_included, subagent: item.thread_kind === 'subagent' }))
      : selectedConversation ? [{ id: selectedConversation.source_id || selectedConversation.id, title: selectedConversation.title || '' }] : [];
    await requestExecutionConfirmation();
  }

  async function requestExecutionConfirmation() {
    if (executionBusy || !executionPreviewID) return;
    const previewID = executionPreviewID;
    const sourceRoot = currentWorkspace?.root;
    executionBusy = true;
    executionMessage = '';
    try {
      const response = await apiFetch('/api/v1/operations/confirmations', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ preview_id: previewID })
      });
      const body = await response.json() as ExecutionConfirmation & { error?: string };
      if (sourceRoot !== currentWorkspace?.root || previewID !== executionPreviewID) return;
      if (!response.ok) throw new Error(body.error || '无法生成确认，请重新预览');
      executionChallenge = body;
      executionOpen = true;
    } catch (error) {
      if (sourceRoot !== currentWorkspace?.root || previewID !== executionPreviewID) return;
      executionMessage = error instanceof Error ? error.message : '无法获取确认数字';
      if (executionOrigin === 'batch') batchOperationMessage = executionMessage; else operationMessage = executionMessage;
    } finally { if (sourceRoot === currentWorkspace?.root) executionBusy = false; }
  }

  async function submitExecutionConfirmation(codes: { code: string; additionalCode: string }) {
    if (executionBusy || !executionChallenge) return;
    const sourceRoot = currentWorkspace?.root;
    const previewID = executionPreviewID;
    executionBusy = true;
    executionMessage = '';
    try {
      const response = await apiFetch('/api/v1/operations/executions', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ confirmation_id: executionChallenge.confirmation_id, confirmation_code: codes.code, additional_code: codes.additionalCode, archive_password: executionPassword })
      });
      const body = await response.json() as ExecutionTask & { error?: string };
      if (sourceRoot !== currentWorkspace?.root) return;
      if (!response.ok) throw new Error(body.error || '服务端未接受执行，请重新预览');
      executionTask = body;
      executionTasks = [body, ...executionTasks.filter(task => task.id !== body.id)];
      submittedExecutionID = body.id;
      executionResult = null;
      operationExecuting = body.state === 'running';
      batchOperationExecuting = operationExecuting;
      impactPreview = null;
      batchOperationPreview = null;
      pruneLatestBatchSelection();
      cancelExecutionConfirmation();
      if (executionOrigin === 'batch') batchOperationMessage = '任务已提交，可在这里或待处理页面查看进度。'; else operationMessage = '任务已提交，可在这里或待处理页面查看进度。';
      await loadExecutionTasks();
    } catch (error) {
      if (sourceRoot !== currentWorkspace?.root) return;
      executionMessage = error instanceof Error ? error.message : '执行请求未返回结果，请刷新任务列表核对';
      // 响应丢失可能发生在任务已经启动之后；先查询持久化记录，禁止直接创建第二个任务。
      await loadExecutionTasks();
      if (executionTasks.some(task => task.preview_id === previewID)) cancelExecutionConfirmation();
    } finally { if (sourceRoot === currentWorkspace?.root) executionBusy = false; }
  }

  async function loadExecutionTasks() {
    if (executionPolling) return;
    const requestToken = ++executionTasksRequestToken;
    const sourceRoot = currentWorkspace?.root;
    executionPolling = true;
    if (executionPollTimer) clearTimeout(executionPollTimer);
    try {
      const response = await apiFetch('/api/v1/operations/executions?view=progress');
      const body = await response.json() as { items?: ExecutionTask[]; errors?: string[]; error?: string };
      if (requestToken !== executionTasksRequestToken || sourceRoot !== currentWorkspace?.root) return;
      if (!response.ok) throw new Error(body.error || '读取执行任务失败');
      if (body.errors?.length) executionMessage = body.errors.join('；');
      const previousTask = executionTask;
      executionTasks = body.items || [];
      executionTask = executionTasks.find(task => task.state === 'running') || executionTasks[0] || null;
      operationExecuting = executionTasks.some(task => task.state === 'running');
      batchOperationExecuting = operationExecuting;
      reconcileExecutionSelection();
      if (executionTask && executionTask.state !== 'running' && (executionTask.id === submittedExecutionID || previousTask?.id === executionTask.id && previousTask.state === 'running')) {
        executionResult = executionTask;
        submittedExecutionID = '';
        batchOperationMessage = '';
        operationMessage = '';
        if (executionTask.state === 'completed' && selectedConversationIDs.size === 0) batchPanelOpen = false;
      }
      if (executionTask && executionTask.state !== 'running' && reconciledExecutionID !== executionTask.id) {
        reconciledExecutionID = executionTask.id;
        void refreshExecutionInventory(sourceRoot || '', executionTask.id);
        void loadRecoveryItems();
      }
    } catch (error) { if (requestToken === executionTasksRequestToken && sourceRoot === currentWorkspace?.root) executionMessage = error instanceof Error ? error.message : '任务状态暂不可读，请刷新核对'; }
    finally {
      if (requestToken === executionTasksRequestToken) {
        executionPolling = false;
        if (executionTasks.some(task => task.state === 'running')) executionPollTimer = setTimeout(() => void loadExecutionTasks(), 500);
      }
    }
  }

  // 后台同步统计和列表，不重置阅读位置、不因活动会话使快照 stale 而追加第二次全量刷新。
  async function refreshExecutionInventory(root: string, taskID: string) {
    try {
      const response = await apiFetch('/api/v1/inventory?offset=0&limit=1');
      if (!response.ok) throw new Error(`读取更新统计失败（HTTP ${response.status}）`);
      const updated = await response.json() as InventoryReport;
      if (root !== currentWorkspace?.root || taskID !== reconciledExecutionID || updated.root !== root) return;
      report = { ...updated, conversations: report?.conversations || [] };
      await loadConversations(true, true);
      if (activeWorkspace === 'pending') void loadPendingConversations();
    } catch (error) {
      if (root === currentWorkspace?.root && taskID === reconciledExecutionID) {
        executionMessage = `执行结果已保存，列表同步暂未完成：${error instanceof Error ? error.message : '请稍后重试'}`;
      }
    }
  }

  async function stopExecutionTask(task: ExecutionTask) {
    try {
      const response = await apiFetch(`/api/v1/operations/executions/${encodeURIComponent(task.id)}/stop`, { method: 'POST' });
      const body = await response.json() as ExecutionTask & { error?: string };
      if (!response.ok) throw new Error(body.error || '停止请求失败');
      executionMessage = '停止请求已保存，正在执行的项会先结束。';
      await loadExecutionTasks();
    } catch (error) { executionMessage = error instanceof Error ? error.message : '无法停止后续项'; }
  }

  function copyDefaultDestination() {
    const root = currentWorkspace?.root || report?.root || '';
    if (!root || !root.toLowerCase().endsWith('/.codex') && !root.toLowerCase().endsWith('\\.codex')) return '';
    return `${root.slice(0, root.length - '.codex'.length)}codex-chronicle-copy`;
  }

  function copyMarker() {
    // 当前工作区的只读 inventory 不能替代完整 schema/语义校验；副本默认保持未绑定状态。
    return { kind: 'codex-real-copy-v1' };
  }

  function copyExcludePaths() {
    return copyExcludePathsText.split(/\r?\n/).map((path) => path.trim()).filter(Boolean);
  }

  async function startCopyPrepare() {
    const destination = (copyDestination.trim() || copyDefaultDestination()).trim();
    if (!destination) {
      copyMessage = '请输入 .codex 外部的副本目标目录';
      return;
    }
    copyDestination = destination;
    copyLoading = true;
    copyMessage = '';
    copyTask = null;
    const token = ++copyPollingToken;
    try {
      const response = await apiFetch('/api/v1/copies', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ destination_root: destination, exclude_paths: copyExcludePaths(), marker: copyMarker(), allow_unavailable_process_probe: copyAllowUnavailableProcessProbe })
      });
      const body = await response.json() as { task?: CopyTask; error?: string };
      if (!response.ok || !body.task) throw new Error(body.error || `生成副本计划失败（HTTP ${response.status}）`);
      copyTask = body.task;
      await pollCopyTask(body.task.id, token);
    } catch (error) {
      if (token === copyPollingToken) copyMessage = error instanceof Error ? error.message : '无法生成稳定副本计划';
    } finally {
      if (token === copyPollingToken) copyLoading = false;
    }
  }

  async function executeCopy() {
    if (!copyTask?.id || copyTask.kind !== 'copy_prepare' || copyTask.status !== 'completed') return;
    copyLoading = true;
    copyMessage = '';
    const token = ++copyPollingToken;
    try {
      const response = await apiFetch(`/api/v1/copies/${encodeURIComponent(copyTask.id)}/execute`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}'
      });
      const body = await response.json() as { task?: CopyTask; error?: string };
      if (!response.ok || !body.task) throw new Error(body.error || `开始制作副本失败（HTTP ${response.status}）`);
      copyTask = body.task;
      await pollCopyTask(body.task.id, token);
      if (copyTask?.status === 'completed') copyMessage = '稳定副本已发布，可以执行完整验证。';
    } catch (error) {
      if (token === copyPollingToken) copyMessage = error instanceof Error ? error.message : '无法制作稳定副本';
    } finally {
      if (token === copyPollingToken) copyLoading = false;
    }
  }

  async function verifyCopy() {
    const destination = copyDestination.trim();
    if (!destination) return;
    copyLoading = true;
    copyMessage = '';
    const token = ++copyPollingToken;
    try {
      const response = await apiFetch('/api/v1/copies/verify', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ root: destination })
      });
      const body = await response.json() as { task?: CopyTask; error?: string };
      if (!response.ok || !body.task) throw new Error(body.error || `开始副本验证失败（HTTP ${response.status}）`);
      copyTask = body.task;
      await pollCopyTask(body.task.id, token);
      if (copyTask?.status === 'completed') copyMessage = '副本 manifest 和全部文件哈希已验证。';
    } catch (error) {
      if (token === copyPollingToken) copyMessage = error instanceof Error ? error.message : '无法验证副本';
    } finally {
      if (token === copyPollingToken) copyLoading = false;
    }
  }

  async function pollCopyTask(id: string, token: number) {
    while (token === copyPollingToken) {
      const response = await apiFetch(`/api/v1/copies/${encodeURIComponent(id)}`);
      const body = await response.json() as { task?: CopyTask; error?: string };
      if (!response.ok || !body.task) throw new Error(body.error || `读取副本任务失败（HTTP ${response.status}）`);
      copyTask = body.task;
      if (['completed', 'failed', 'cancelled'].includes(body.task.status)) {
        if (body.task.status !== 'completed' && body.task.error) copyMessage = body.task.error;
        return;
      }
      await new Promise((resolve) => setTimeout(resolve, 350));
    }
  }

  function formatBytes(bytes: number) {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
    return `${(bytes / 1024 / 1024 / 1024).toFixed(2)} GB`;
  }

  function formatTokens(tokens: number) {
    if (!tokens) return '—';
    return new Intl.NumberFormat('zh-CN', { notation: 'compact', maximumFractionDigits: 1 }).format(tokens);
  }

  function formatNumber(value: number) {
    return new Intl.NumberFormat('zh-CN').format(value);
  }

  function buildHomeTrend(daily: AnalyticsReport['daily']): HomeTrendPoint[] {
    const byDate = new Map(daily.map((point) => [point.date, point]));
    const today = new Date();
    today.setUTCHours(0, 0, 0, 0);
    return Array.from({ length: 14 }, (_, index) => {
      const date = new Date(today.getTime() - (13 - index) * 86_400_000).toISOString().slice(0, 10);
      const point = byDate.get(date);
      return { date, label: date.slice(5).replace('-', '/'), total_tokens: point?.total_tokens ?? 0, conversations: point?.conversations ?? 0, messages: point?.messages ?? 0, turns: point?.turns ?? 0 };
    });
  }

  const graphKindLabels: Record<string, string> = {
    conversation: '逻辑对话',
    thread: '线程',
    rollout: 'rollout',
    file: '文件',
    sqlite_relation: 'SQLite 关系',
    sqlite_row: 'SQLite 行',
    index_entry: '索引条目'
  };
  const graphCompletenessLabels: Record<string, string> = {
    complete: '关系完整',
    partial: '关系不完整',
    unknown: '关系未知'
  };

  function graphNodeCounts(graph?: ConversationAssetGraph) {
    const counts = new Map<string, number>();
    for (const node of graph?.nodes ?? []) counts.set(node.kind, (counts.get(node.kind) ?? 0) + 1);
    return Object.entries(graphKindLabels)
      .map(([kind, label]) => ({ kind, label, count: counts.get(kind) ?? 0 }))
      .filter((item) => item.count > 0);
  }

  function graphNodeLabel(graph: ConversationAssetGraph | undefined, id: string) {
    const node = graph?.nodes.find((item) => item.id === id);
    if (!node) return id;
    return node.relative_path || node.label || node.thread_id || node.row_key || node.id;
  }

  function graphNodeDetail(node: AssetGraphNode) {
    const location = node.relative_path ? `${node.relative_path}${node.line ? `:${node.line}` : ''}` : '';
    const table = node.table ? `${node.table}${node.column ? `.${node.column}` : ''}` : '';
    return [location, table, node.thread_id, node.row_key ? `${node.row_key_kind || '行'}=${node.row_key}` : ''].filter(Boolean).join(' · ');
  }

  function formatTime(value?: string) {
    return value ? new Date(value).toLocaleString('zh-CN') : '时间未知';
  }

  function formatConversationDate(value?: string) {
    const date = new Date(value || '');
    if (Number.isNaN(date.getTime())) return '时间未知';
    const today = new Date();
    if (date.toDateString() === today.toDateString()) return date.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' });
    return date.toLocaleDateString('zh-CN', { ...(date.getFullYear() !== today.getFullYear() ? { year: 'numeric' } : {}), month: 'numeric', day: 'numeric' });
  }
</script>

<svelte:head>
  <title>Codex Chronicle · 本地工作台</title>
</svelte:head>

{#if authRequired}
  <main class="auth-gate">
    <section class="auth-card">
      <div class="brand-mark auth-brand">CC</div>
      <div class="eyebrow">Codex Chronicle</div>
      <h1>需要验证本地访问</h1>
      <p class="auth-note">daemon 已启用远程访问保护。会话令牌只保存在当前浏览器会话中。</p>
      <label>设备名称<input bind:value={authDevice} maxlength="128" autocomplete="off" placeholder="例如：我的 Mac 浏览器" /></label>
      {#if !authBound}
        <form class="auth-form" on:submit|preventDefault={bindRemote}>
          <label>一次性绑定码<input bind:value={authCode} required autocomplete="one-time-code" placeholder="输入 daemon 启动日志中的绑定码" /></label>
          <label>本地账户密码<input bind:value={authPassword} required type="password" autocomplete="new-password" placeholder="设置后可用于后续登录" /></label>
          <button class="auth-primary" type="submit" disabled={authLoading || !authCode.trim() || !authPassword}>{authLoading ? '绑定中…' : '绑定此设备'}</button>
        </form>
        {#if bindingExpired()}<button class="auth-secondary" type="button" on:click={reissueBinding} disabled={authLoading}>重新生成绑定码</button>{/if}
      {:else}
        <form class="auth-form" on:submit|preventDefault={loginRemote}>
          <label>本地账户密码<input bind:value={authPassword} required type="password" autocomplete="current-password" placeholder="输入绑定时设置的密码" /></label>
          <button class="auth-primary" type="submit" disabled={authLoading || !authPassword}>{authLoading ? '登录中…' : '登录'}</button>
        </form>
      {/if}
      {#if authMessage}<div class="auth-message">{authMessage}</div>{/if}
      <small class="auth-help">绑定成功后，后续可使用本地账户密码获取新的会话令牌。</small>
    </section>
  </main>
{:else}
<div class="app-shell" class:session-page={activeWorkspace === 'sessions' && !routeNotFoundPath} class:density-compact={density === 'compact'} class:density-reading={density === 'reading'}>
  <header class="topbar">
    <div class="brand-mark">CC</div>
    <div>
      <div class="brand-name">Codex Chronicle</div>
      <h1>对话档案库</h1>
    </div>
    <form class="global-search" on:submit|preventDefault={submitSearch}>
      <span class="search-icon" aria-hidden="true">⌕</span>
      <input bind:value={searchDraft} aria-label="搜索对话正文和元数据" aria-keyshortcuts="/ Control+K Meta+K" placeholder="搜索对话、正文或路径" />
      {#if searchDraft || search}<button class="search-clear" type="button" aria-label="清除搜索" title="清除搜索" on:click={() => void clearSearch()}>×</button>{/if}
      <span class="search-shortcut" title="聚焦搜索">⌘K</span>
      <button type="submit" disabled={searching}>{searching ? '搜索中…' : '搜索'}</button>
    </form>
    <span class="readonly-badge" class:write-enabled={lifecycleWritesEnabled} title={lifecycleWritesEnabled && lifecycleFixtureOnly ? '写入仅限明确指定的测试样本或稳定副本，真实 .codex 保持只读' : undefined}>
      <span class="status-dot"></span>{lifecycleWritesEnabled ? (lifecycleFixtureOnly ? '测试样本写入' : lifecycleRealConversations ? '本机会话执行' : '受限真实执行') : '只读模式'}
    </span>
    <button class="secondary-button refresh-button" title="重新扫描源目录，刷新会话列表" on:click={() => refreshInventory()} disabled={loading}>{loading ? '刷新中…' : '刷新'}</button>
    {#if executionTasks.length || executionMessage}<button class="secondary-button execution-task-button" type="button" title="查看执行进度和恢复材料" on:click={() => selectWorkspaceView('pending')}><Inbox aria-hidden="true" /><span>{operationExecuting ? '执行中' : '执行任务'}</span></button>{/if}
    <ThemeSwitcher />
    <nav class="primary-nav" aria-label="主要工作区">
      {#each workspaceNavigation as item}
        <button class:active={!routeNotFoundPath && activeWorkspace === item.id} aria-current={!routeNotFoundPath && activeWorkspace === item.id ? 'page' : undefined} title={item.description} on:click={() => selectWorkspaceView(item.id)}><NavigationIcon name={item.id} /><span>{item.id === 'settings' ? '设置' : item.label}</span></button>
      {/each}
    </nav>
    <nav class="compact-pane-tabs" class:session-pane-tabs={activeWorkspace === 'sessions'} aria-label="会话库视图">
      <button class:active={compactPane === 'workspace'} aria-pressed={compactPane === 'workspace'} on:click={() => compactPane = 'workspace'}>工作区</button>
      <button class:active={compactPane === 'sessions'} aria-pressed={compactPane === 'sessions'} on:click={() => selectWorkspaceView('sessions')}>对话列表</button>
      <button class:active={compactPane === 'detail'} aria-pressed={compactPane === 'detail'} on:click={() => selectedConversation && navigateToRoute({ page: 'session', conversationID: selectedConversation.id }, false)} disabled={!selectedConversation}>对话详情</button>
    </nav>
  </header>

  {#if routeNotFoundPath}
    <main class="workspace-view route-not-found" role="alert">
      <div><span class="eyebrow">页面不存在</span><h2>无法识别这个地址</h2><p title={routeNotFoundPath}>{routeNotFoundPath}</p><button type="button" class="secondary-button" on:click={() => selectWorkspaceView('home')}>返回工作台</button></div>
    </main>
  {:else if activeWorkspace === 'home'}
    <main class="workspace-view workspace-home">
      {#if report?.analytics}
        <div class="home-dashboard">
          <header class="workspace-view-heading home-view-heading">
            <div><span class="eyebrow">本地数据总览</span><h2>工作台</h2></div>
            <div class="home-source-meta">
              <span class="home-source" title={report.root}>{currentWorkspace?.name || formatPath(report.root)}</span>
              <span class="status-pill" class:stable={!report.stale && report.snapshot_status !== 'stale'} class:needs_review={report.stale || report.snapshot_status === 'stale'}>{loading ? '正在扫描' : report.stale || report.snapshot_status === 'stale' ? '数据待刷新' : '只读数据'}</span>
              <time datetime={report.last_successful_scan_at || report.generated_at}>更新于 {formatTime(report.last_successful_scan_at || report.generated_at)}</time>
            </div>
          </header>

          <section class="home-metrics" aria-label="工作区概览">
            <button type="button" class="home-metric" on:click={() => selectWorkspaceView('sessions')}>
              <span class="home-metric-label"><MessagesSquare aria-hidden="true" />逻辑对话</span>
              <strong>{formatNumber(report.analytics.conversation_count)}</strong>
              <small>{formatNumber(report.analytics.stable_count)} 稳定 · {formatNumber(report.analytics.blocked_count)} 需关注</small>
            </button>
            <button type="button" class="home-metric" on:click={() => selectWorkspaceView('analytics')}>
              <span class="home-metric-label"><Activity aria-hidden="true" />累计 tokens</span>
              <strong>{report.analytics.token_usage.total_tokens ? formatTokens(report.analytics.token_usage.total_tokens) : '0'}</strong>
              <small>{formatNumber(report.analytics.message_count)} 条消息 · {formatNumber(report.analytics.turn_count)} 回合</small>
            </button>
            <button type="button" class="home-metric" on:click={() => selectWorkspaceView('maintenance')}>
              <span class="home-metric-label"><Database aria-hidden="true" />源目录</span>
              <strong>{report.analytics.total_bytes ? formatBytes(report.analytics.total_bytes) : '0 B'}</strong>
              <small>{formatNumber(report.analytics.file_count)} 个物理资产</small>
            </button>
            <button type="button" class="home-metric" class:home-metric-attention={report.analytics.blocked_count > 0} on:click={() => selectWorkspaceView('pending')}>
              <span class="home-metric-label"><TriangleAlert aria-hidden="true" />非稳定对话</span>
              <strong>{formatNumber(report.analytics.blocked_count)}</strong>
              <small>稳定状态之外的会话</small>
            </button>
          </section>

          <div class="home-content-grid">
            <section class="home-panel home-recent-panel">
              <header class="home-panel-heading"><div><h3>最近对话</h3><span>按更新时间排序</span></div><button type="button" class="home-panel-action" on:click={() => selectWorkspaceView('sessions')}>全部对话 <ArrowRight aria-hidden="true" /></button></header>
              {#if recentConversations.length}
                <div class="home-recent-list">
                  {#each recentConversations as conversation (conversation.id)}
                    <button type="button" class="home-recent-row" on:click={() => openConversationFromWorkspace(conversation)}>
                      <span class="home-recent-copy">
                        <span class="home-recent-title"><strong title={conversation.title || '未命名对话'}>{conversation.title || '未命名对话'}</strong>{#if conversation.status !== 'stable' || conversation.diagnostics?.length}<span class={`status-pill ${conversation.status === 'stable' ? 'needs_review' : conversation.status}`}>{conversation.status === 'stable' ? '需复核' : statusLabels[conversation.status] || '需复核'}</span>{/if}</span>
                        <span class="home-recent-preview">{conversation.first_user_text || '暂无首条消息摘要'}</span>
                        <span class="home-recent-meta"><span title={conversation.cwd || undefined}>{formatPath(conversation.cwd)}</span><time datetime={conversation.updated_at || conversation.started_at}>{formatTime(conversation.updated_at || conversation.started_at)}</time></span>
                      </span>
                      <span class="home-recent-tokens">{conversation.token_usage.total_tokens ? formatTokens(conversation.token_usage.total_tokens) : '0'}<small>tokens</small></span>
                    </button>
                  {/each}
                </div>
              {:else if conversationsLoading}
                <div class="home-state" role="status">正在读取最近对话…</div>
              {:else}
                <div class="home-state"><strong>{conversationTotal ? '最近对话暂不可用' : '当前工作区还没有对话'}</strong><span>{errorMessage || (conversationTotal ? '可到会话库查看完整列表。' : '扫描 Codex 对话后会显示在这里。')}</span><button type="button" class="secondary-button" on:click={() => selectWorkspaceView('sessions')}>打开会话库</button></div>
              {/if}
            </section>

            <div class="home-side-stack">
              <section class="home-panel home-trend-panel">
                <header class="home-panel-heading"><div><h3>每日 token</h3><span>最近 14 天 · 按 UTC 对话更新时间</span></div><button type="button" class="home-icon-action" aria-label="打开统计分析" title="打开统计分析" on:click={() => selectWorkspaceView('analytics')}><ArrowRight aria-hidden="true" /></button></header>
                {#if homeTrendPoints.some((point) => point.conversations > 0)}
                  <div class="home-trend-chart" role="img" aria-label={`最近 14 天共 ${formatTokens(homeTrendPoints.reduce((sum, point) => sum + point.total_tokens, 0))} tokens，${formatNumber(homeTrendPoints.filter((point) => point.conversations > 0).length)} 天有对话记录`}>
                    {#each homeTrendPoints as point}
                      <span class="home-trend-column" title={`${point.date} · ${formatNumber(point.conversations)} 个对话 · ${point.total_tokens ? formatTokens(point.total_tokens) : '0'} tokens`}>
                        <i class:empty={point.total_tokens === 0} style={`height: ${point.total_tokens ? Math.max(3, point.total_tokens / homeTrendPeak * 100) : 2}%`}></i>
                      </span>
                    {/each}
                  </div>
                  <div class="home-trend-axis"><span>{homeTrendPoints[0].label}</span><span>{homeTrendPoints[6].label}</span><span>{homeTrendPoints[13].label}</span></div>
                  <p class="home-trend-summary">14 天累计 {homeTrendPoints.reduce((sum, point) => sum + point.total_tokens, 0) ? formatTokens(homeTrendPoints.reduce((sum, point) => sum + point.total_tokens, 0)) : '0'} tokens</p>
                  <details class="home-trend-details"><summary>每日明细</summary><div>{#each homeTrendPoints as point}<span><time>{point.date}</time><strong>{point.total_tokens ? formatTokens(point.total_tokens) : '0'}</strong><small>{point.conversations} 个对话</small></span>{/each}</div></details>
                {:else}
                  <div class="home-state home-trend-empty">最近 14 天没有对话记录。</div>
                {/if}
              </section>

              <section class="home-panel home-project-panel">
                <header class="home-panel-heading"><div><h3>活跃项目</h3><span>{formatNumber(homeProjects.length)} 个项目</span></div><button type="button" class="home-icon-action" aria-label="打开项目统计" title="打开项目统计" on:click={() => selectWorkspaceView('analytics')}><ArrowRight aria-hidden="true" /></button></header>
                {#if homeProjects.length}
                  <div class="home-project-list">
                    {#each homeProjects as project}
                      <button type="button" class="home-project-row" on:click={() => drillIntoAnalytics('project', project.name)}>
                        <span><FolderOpen aria-hidden="true" /><strong title={project.name}>{formatPath(project.name)}</strong></span>
                        <small>{formatNumber(project.count)} 个对话</small>
                      </button>
                    {/each}
                  </div>
                {:else}
                  <div class="home-state home-project-empty">尚无可归类的项目目录。</div>
                {/if}
              </section>
            </div>
          </div>
        </div>
      {:else}
        <section class="workspace-card-panel home-unavailable">
          <h2>{loading ? '正在读取工作区' : errorMessage ? '工作区暂不可用' : '等待工作区数据'}</h2>
          <p>{errorMessage || '连接本地 daemon 并完成只读扫描后，概览数据会显示在这里。'}</p>
          {#if errorMessage}<button type="button" class="secondary-button" on:click={() => refreshInventory()}>重试读取</button>{/if}
        </section>
      {/if}
    </main>
  {:else if activeWorkspace === 'pending'}
    <main class="workspace-view pending-view">
      <div class="pending-workspace">
        {#if executionTasks.length || executionMessage}<section aria-label="最近执行任务"><h2>执行任务</h2>{#if executionMessage}<p class="pending-feedback" role="status">{executionMessage}<button type="button" on:click={loadExecutionTasks}>重新读取</button></p>{/if}{#each executionTasks.slice(0, 10) as task (task.id)}<ExecutionProgress {task} on:stop={() => stopExecutionTask(task)} on:refresh={loadExecutionTasks} />{/each}</section>{/if}
        <header class="pending-page-heading">
          <div><h2>待处理</h2><span title={report?.root}>{currentWorkspace?.name || '当前工作区'}<span class="pending-heading-note"> · 逐项查看，再决定下一步</span></span></div>
          <button type="button" class="secondary-button pending-refresh" on:click={refreshPendingWorkspace} disabled={pendingConversationLoading || pendingLoading || recoveryLoading}><RefreshCw aria-hidden="true" class={pendingConversationLoading || pendingLoading || recoveryLoading ? 'pending-spinning' : ''} />{pendingConversationLoading || pendingLoading || recoveryLoading ? '刷新中…' : '刷新队列'}</button>
        </header>
        <nav class="pending-queue-tabs" aria-label="待处理队列">
          <button type="button" class:active={pendingTab === 'conversations'} aria-current={pendingTab === 'conversations' ? 'page' : undefined} on:click={() => selectPendingTab('conversations')}><MessagesSquare aria-hidden="true" /><span>需关注会话</span><small>{pendingConversationLoading && !pendingConversationItems.length ? '…' : pendingConversationTotal}</small></button>
          <button type="button" class:active={pendingTab === 'operations'} aria-current={pendingTab === 'operations' ? 'page' : undefined} on:click={() => selectPendingTab('operations')}><Inbox aria-hidden="true" /><span>暂存操作</span><small>{pendingOpenCount}</small></button>
          <button type="button" class:active={pendingTab === 'recovery'} aria-current={pendingTab === 'recovery' ? 'page' : undefined} on:click={() => selectPendingTab('recovery')}><ShieldCheck aria-hidden="true" /><span>恢复任务</span><small>{recoveryItems.length}</small></button>
        </nav>
        <section class="pending-queue-panel" aria-label={pendingTab === 'conversations' ? '需关注会话' : pendingTab === 'operations' ? '暂存操作' : '恢复任务'}>
          <form class="pending-toolbar" on:submit|preventDefault={applyPendingFilters}>
            <label class="pending-search"><Search aria-hidden="true" /><input bind:value={pendingSearchDraft} aria-label="搜索当前待处理队列" placeholder={pendingTab === 'conversations' ? '搜索标题、项目或会话 ID' : '搜索会话、操作 ID 或原因'} /><button type="submit" aria-label="搜索队列"><ArrowRight aria-hidden="true" /></button></label>
            <label class="pending-state-filter"><span class="sr-only">队列状态</span><select bind:value={pendingFilter} aria-label="队列状态" on:change={applyPendingFilters}>
              {#if pendingTab === 'conversations'}
                <option value="">全部关注状态</option>{#each Object.entries(statusLabels) as [state, label]}<option value={state}>{state === 'stable' ? '稳定但有诊断' : label}</option>{/each}
              {:else if pendingTab === 'operations'}
                <option value="">未结束</option><option value="waiting_for_idle">等待空闲</option><option value="idle_detected">已检测空闲</option><option value="confirmation_required">等待确认</option><option value="revalidation_failed">资产已变化</option><option value="needs_review">人工复核</option><option value="ready_for_fixture_execution">副本待执行</option><option value="history">已完成与已取消</option><option value="all">全部记录</option>
              {:else}
                <option value="">全部恢复任务</option><option value="rollback">可回滚</option><option value="review">需人工检查</option>
              {/if}
            </select></label>
            {#if pendingQuery || pendingFilter}<button type="button" class="pending-text-button" on:click={clearPendingFilters}>清除条件</button>{/if}
          </form>
          {#if pendingTab === 'conversations'}
            <div class="pending-queue-caption"><span>非稳定状态或包含诊断的会话</span><small>活动写入不代表损坏</small></div>
            {#if pendingConversationError}<div class="pending-feedback error" role="alert"><span>{pendingConversationError}{pendingConversationItems.length ? '；下方保留上次结果。' : ''}</span><button type="button" on:click={() => loadPendingConversations()}>重试</button></div>{/if}
            {#if pendingConversationStale}<div class="pending-feedback warning" role="status">源数据正在变化，当前显示上次扫描结果。<button type="button" on:click={() => refreshInventory()}>更新扫描</button></div>{/if}
            {#if pendingConversationItems.length}
              <div class="pending-rows">
                {#each pendingConversationItems as conversation (conversation.id)}
                  <button type="button" class="pending-session-row" on:click={() => openConversationFromWorkspace(conversation)}>
                    <span class="pending-row-copy"><span class="pending-row-heading"><strong title={conversation.title || '未命名对话'}>{conversation.title || '未命名对话'}</strong><span class={'status-pill ' + (conversation.status === 'stable' ? 'needs_review' : conversation.status)}>{conversation.status === 'stable' ? '有诊断' : statusLabels[conversation.status] || conversation.status}</span></span><span class="pending-row-preview">{conversation.diagnostics?.[0]?.message || conversation.first_user_text || '打开会话查看内容与证据'}</span><span class="pending-row-meta"><span title={conversation.cwd}>{projectLabel(conversation.cwd || '未关联项目')}</span><time datetime={conversation.updated_at || conversation.started_at}>{formatTime(conversation.updated_at || conversation.started_at)}</time>{#if conversation.diagnostics?.length}<span>{conversation.diagnostics.length} 条诊断</span>{/if}</span></span>
                    <span class="pending-row-trailing"><span>{formatTokens(conversation.token_usage.total_tokens)}<small>tokens</small></span><ChevronRight aria-hidden="true" /></span>
                  </button>
                {/each}
              </div>
            {:else if pendingConversationLoading}<div class="pending-empty" role="status"><RefreshCw class="pending-spinning" aria-hidden="true" /><strong>正在读取需关注会话…</strong></div>
            {:else if !pendingConversationError}<div class="pending-empty"><ShieldCheck aria-hidden="true" /><strong>{pendingQuery || pendingFilter ? '没有匹配的会话' : '当前没有需关注会话'}</strong><span>{pendingQuery || pendingFilter ? '试试其他关键词，或清除筛选条件。' : '按当前扫描结果，未发现非稳定或带诊断的会话。'}</span><button type="button" class="secondary-button" on:click={() => pendingQuery || pendingFilter ? clearPendingFilters() : selectWorkspaceView('sessions')}>{pendingQuery || pendingFilter ? '清除条件' : '打开会话库'}</button></div>{/if}
            <footer class="pending-list-footer"><span aria-live="polite">已载入 {pendingConversationItems.length} / {pendingConversationTotal} 项</span>{#if pendingConversationItems.length < pendingConversationTotal}<button type="button" class="secondary-button" disabled={pendingConversationLoading} on:click={() => loadPendingConversations(true)}>{pendingConversationLoading ? '读取中…' : '加载更多'}</button>{/if}</footer>
          {:else if pendingTab === 'operations'}
            <div class="pending-queue-caption"><span>保存的处理意图不会自动执行</span><small>{filteredPendingOperations.length} / {pendingOperations.length} 条记录</small></div>
            {#if pendingLoadError}<div class="pending-feedback error" role="alert"><span>{pendingLoadError}{pendingOperations.length ? '；下方保留上次结果。' : ''}</span><button type="button" on:click={loadPendingOperations}>重试</button></div>{/if}
            {#if pendingMessage}<div class="pending-feedback" role="status">{pendingMessage}</div>{/if}
            {#if filteredPendingOperations.length}
              <div class="pending-rows">
                {#each filteredPendingOperations as operation (operation.operation_id)}
                  <article class="pending-intent-row">
                    <button type="button" class="pending-intent-open" on:click={() => inspectPendingOperation(operation)}><span class="pending-row-heading"><strong title={pendingConversation(operation)?.title || operation.conversation_id}>{pendingConversation(operation)?.title || operation.conversation_id}</strong><span class={'status-pill ' + operation.state}>{pendingStateLabels[operation.state] || operation.state}</span>{#if pendingExpired(operation) && !['completed', 'cancelled'].includes(operation.state)}<span class="status-pill needs_review">已过期</span>{/if}</span><span class="pending-row-preview">{operationSummary(operation.strategy)}</span><span class="pending-row-meta"><span>{operation.asset_set.length} 个资产</span><time datetime={operation.last_probe_at || operation.created_at}>{formatTime(operation.last_probe_at || operation.created_at)}</time><span>{pendingNextStep(operation)}</span></span></button>
                    <button type="button" class="pending-text-button" on:click={() => inspectPendingOperation(operation)}>查看详情 <ChevronRight aria-hidden="true" /></button>
                  </article>
                {/each}
              </div>
            {:else if pendingLoading}<div class="pending-empty" role="status"><RefreshCw class="pending-spinning" aria-hidden="true" /><strong>正在读取暂存操作…</strong></div>
            {:else if !pendingLoadError}<div class="pending-empty"><Inbox aria-hidden="true" /><strong>{pendingQuery || pendingFilter ? '没有匹配的暂存操作' : '没有未结束的暂存操作'}</strong><span>{pendingQuery || pendingFilter ? '调整关键词或状态筛选后重试。' : '可在会话库查看影响预览后，保存暂时无法继续的处理意图。'}</span><button type="button" class="secondary-button" on:click={() => pendingQuery || pendingFilter ? clearPendingFilters() : selectWorkspaceView('sessions')}>{pendingQuery || pendingFilter ? '清除条件' : '打开会话库'}</button></div>{/if}
          {:else}
            <div class="pending-queue-caption"><span>先检查中断状态与回执，再决定恢复方式</span><button type="button" class="pending-text-button" on:click={openRecoverySettings}>打开恢复控制 <ArrowRight aria-hidden="true" /></button></div>
            {#if recoveryMessage}<div class="pending-feedback error" role="alert">{recoveryMessage}</div>{/if}
            {#if filteredRecoveryItems.length}
              <div class="pending-rows">
                {#each filteredRecoveryItems as item (item.operation_id)}
                  <article class="pending-recovery-row"><div class="pending-row-copy"><div class="pending-row-heading"><strong title={item.conversation_id || item.operation_id}>{item.conversation_id || item.operation_id}</strong><span class={'status-pill ' + (item.external_state_unknown ? 'needs_review' : 'waiting_for_idle')}>{item.external_state_unknown ? '外部状态待核实' : recoveryPhaseLabels[item.phase] || item.phase}</span></div><p>{item.error_message || item.reason || item.recovery_note || recoveryDispositionLabels[item.disposition] || item.disposition}</p><span class="pending-row-meta"><span>{item.asset_count} 个资产 · {formatBytes(item.bytes)}</span><span>{item.receipt_count} 个回执</span><time datetime={item.updated_at}>{formatTime(item.updated_at)}</time></span></div><button type="button" class="pending-text-button" on:click={openRecoverySettings}>检查恢复 <ChevronRight aria-hidden="true" /></button></article>
                {/each}
              </div>
            {:else if recoveryLoading}<div class="pending-empty" role="status"><RefreshCw class="pending-spinning" aria-hidden="true" /><strong>正在读取恢复任务…</strong></div>
            {:else if !recoveryMessage}<div class="pending-empty"><ShieldCheck aria-hidden="true" /><strong>{pendingQuery || pendingFilter ? '没有匹配的恢复任务' : recoveryAvailable ? '没有待复核的中断操作' : '当前工作区未配置恢复记录'}</strong><span>{pendingQuery || pendingFilter ? '可以清除筛选条件后查看全部任务。' : '恢复入口保留在归档与恢复设置中。'}</span><button type="button" class="secondary-button" on:click={() => pendingQuery || pendingFilter ? clearPendingFilters() : openRecoverySettings()}>{pendingQuery || pendingFilter ? '清除条件' : '查看归档与恢复'}</button></div>{/if}
          {/if}
        </section>
        <aside class="pending-related" aria-label="相关任务"><button type="button" on:click={() => selectWorkspaceView('maintenance')}><Database aria-hidden="true" /><span>存储维护</span><small>{report?.maintenance?.candidate_count ? formatBytes(report.maintenance.candidate_bytes) + ' 待检查' : '查看维护候选'}</small><ArrowRight aria-hidden="true" /></button><button type="button" on:click={() => selectWorkspaceView('sessions')}><MessagesSquare aria-hidden="true" /><span>批量摘要</span><small>{summaryTask ? summaryTaskStatusLabel(summaryTask.status) + ' · ' + summaryTask.completed + '/' + summaryTask.requested : '在会话库选择对话后生成'}</small><ArrowRight aria-hidden="true" /></button></aside>
      </div>
    </main>
  <LibraryDrawer bind:open={pendingDrawerOpen} title="暂存操作详情" description="查看保存的意图与资产快照。打开详情不会执行源文件操作。">
    {#if pendingSelectedOperation}
      {@const operation = pendingSelectedOperation}
      <div class="pending-inspector">
        <div class="pending-inspector-heading"><strong>{pendingConversation(operation)?.title || operation.conversation_id}</strong><span class={'status-pill ' + operation.state}>{pendingStateLabels[operation.state] || operation.state}</span><p>{operationSummary(operation.strategy)}</p><p>{pendingNextStep(operation)}</p></div>
        {#if pendingMessage}<div class="pending-feedback" role="status">{pendingMessage}</div>{/if}
        {#if pendingExpired(operation) && !['completed', 'cancelled'].includes(operation.state)}<div class="pending-feedback warning">意图已过期，请返回会话重新检查。不会继续预览或确认。</div>{/if}
        {#if operation.block_reasons?.length}<section><h3>当前阻塞原因</h3><div class="pending-operation-reasons">{#each operation.block_reasons as reason}<span>{reason}</span>{/each}</div></section>{/if}
        <dl><dt>会话 ID</dt><dd><code>{operation.conversation_id}</code></dd><dt>创建时间</dt><dd>{formatTime(operation.created_at)}</dd><dt>最近探测</dt><dd>{operation.last_probe_at ? formatTime(operation.last_probe_at) : '尚未探测'}</dd><dt>空闲探测</dt><dd>{operation.probe_window_count || 0} / {operation.required_window_count || 3} 个窗口</dd><dt>有效期至</dt><dd>{operation.expires_at ? formatTime(operation.expires_at) : '未设置'}</dd></dl>
        <details><summary>已保存的资产（{operation.asset_set.length}）</summary><div class="pending-inspector-assets">{#each operation.asset_set as asset}<div><code>{asset.relative_path}</code><span>{formatBytes(asset.size_bytes)} · {asset.kind}</span>{#if asset.sha256}<code>SHA-256: {asset.sha256}</code>{/if}</div>{/each}</div><p>这是暂存时的资产快照，不能代替执行前的完整关联分析和重新校验。</p></details>
        {#if operation.last_activity?.results?.length}<details><summary>活动与锁状态证据（{operation.last_activity.results.length}）</summary><div class="pending-inspector-assets">{#each operation.last_activity.results as item}<div><code>{item.relative_path}</code><span>{statusLabels[item.status] || item.status}</span>{#if item.evidence?.reason}<p>{item.evidence.reason}</p>{/if}{#if item.evidence?.open_by_process?.length}<p>占用进程：{item.evidence.open_by_process.map((process) => process.command || String(process.pid)).join('、')}</p>{/if}{#if item.evidence?.matching_lock_ids?.length}<p>关联锁：{item.evidence.matching_lock_ids.join('、')}</p>{/if}</div>{/each}</div></details>{/if}
        {#if operation.preview}<details open><summary>最近一次影响预览</summary><div><p>计划状态：{operationPlanStatusLabels[operation.preview.status || ''] || operation.preview.status || '未知'}</p><p>动作：{operation.preview.actions?.map((action) => ({ archive: '归档', encrypted_archive: '加密归档', trash: '移入回收站', permanent_delete: '删除', repair: '修复', restore: '恢复' }[action] || action)).join('、') || '没有可执行动作'}</p><div class="pending-inspector-assets">{#each operation.preview.assets || [] as asset}<div><code>{asset.relative_path}</code><span>{formatBytes(asset.size_bytes)}</span><code>SHA-256: {asset.sha256}</code></div>{/each}</div>{#if operation.preview.shared_asset_paths?.length}<p>共享资产：{operation.preview.shared_asset_paths.join('、')}</p>{/if}{#if operation.preview.verification?.length}<p>验证要求：{operation.preview.verification.join('；')}</p>{/if}</div></details>{/if}
        <details><summary>技术标识与来源</summary><dl><dt>完整标题</dt><dd>{pendingConversation(operation)?.title || operation.conversation_id}</dd><dt>操作 ID</dt><dd><code>{operation.operation_id}</code></dd><dt>源目录</dt><dd><code>{operation.source_root}</code></dd><dt>工作区</dt><dd><code>{operation.workspace}</code></dd><dt>资产指纹</dt><dd><code>{operation.asset_set_hash}</code></dd></dl></details>
        {#if !['completed', 'cancelled'].includes(operation.state)}<div class="pending-inspector-actions">
          {#if ['pending', 'waiting_for_idle', 'revalidation_failed', 'needs_review'].includes(operation.state)}<button type="button" class="secondary-button" on:click={() => refreshPendingOperation(operation)} disabled={pendingBusy !== '' || pendingExpired(operation)}>{pendingBusy === operation.operation_id ? '探测中…' : '重新探测空闲'}</button>{/if}
          {#if operation.state === 'idle_detected' || operation.state === 'confirmation_required'}<button type="button" class="secondary-button" on:click={() => repreviewPendingOperation(operation)} disabled={pendingBusy !== '' || pendingExpired(operation)}>{pendingBusy === operation.operation_id ? '生成中…' : '重新生成预览'}</button>{/if}
          <button type="button" class="secondary-button" on:click={() => navigateToRoute({ page: 'session', conversationID: operation.conversation_id })}>阅读源会话</button><button type="button" class="github-danger secondary-button" on:click={() => cancelPendingOperation(operation)} disabled={pendingBusy !== ''}>取消暂存意图</button>
        </div>{/if}
        {#if operation.state === 'confirmation_required' && !pendingExpired(operation)}
          {#if lifecycleFixtureOnly && lifecycleWritesEnabled}<form class="pending-confirm-form" on:submit|preventDefault={() => confirmPendingOperation(operation)}><h3>确认测试副本的暂存计划</h3><p>此处只确认意图，不执行归档或删除。执行前仍须重新校验。</p>
            {#if operation.preview?.requires_confirmation}<label>本次预览确认数字：<code>{pendingPreviewCode || '请先重新生成预览'}</code><input bind:value={pendingConfirmationCode} inputmode="numeric" autocomplete="off" placeholder="输入本次预览的确认数字" aria-label="暂存计划确认数字" /></label>{/if}
            {#if operation.strategy.encrypted}<label>归档密码<input type="password" bind:value={pendingArchivePassword} autocomplete="new-password" /></label>{/if}
            <label class="pending-review-check"><input type="checkbox" bind:checked={pendingReviewed} />我已核对本次预览、资产清单和阻塞原因</label><button type="submit" class="copy-confirm" disabled={pendingBusy !== '' || !pendingReviewed || !operation.preview || (operation.strategy.encrypted && !pendingArchivePassword.trim()) || (operation.preview.requires_confirmation && (!pendingPreviewCode || pendingConfirmationCode.trim() !== pendingPreviewCode))}>{pendingBusy === operation.operation_id ? '确认中…' : '确认暂存计划（不执行）'}</button>
          </form>{:else}<div class="pending-feedback warning">当前工作区仅开放读取、分析和预览。暂存计划的确认入口只对已启用的测试副本开放。</div>{/if}
        {/if}
      </div>
    {:else}<div class="pending-empty"><strong>这条暂存记录已不可用</strong><span>关闭详情后刷新队列，检查最新状态。</span></div>{/if}
  </LibraryDrawer>
  {:else if activeWorkspace === 'analytics'}
    <AnalyticsPage
      overview={report?.analytics ?? null}
      sourceRoot={currentWorkspace?.root || report?.root || ''}
      sourceName={currentWorkspace?.name || '当前工作区'}
      snapshot={report?.generated_at || ''}
      request={apiFetch}
      onInspect={openAnalyticsConversations}
      onRescan={() => { void loadInventory(true); }}
      {statusLabels}
    />
  {:else if activeWorkspace === 'maintenance'}
    {#key `${currentWorkspace?.id || ''}:${currentWorkspace?.root || report?.root || ''}`}
      <MaintenancePage
        sourceRoot={currentWorkspace?.root || report?.root || ''}
        sourceName={currentWorkspace?.name || '当前工作区'}
        snapshot={report?.generated_at || ''}
        request={apiFetch}
        confirmDestination={checkExecutionDestinations}
        onRescan={() => { void refreshInventory(); }}
        onChanged={async () => { await refreshInventory(true); await loadRecoveryItems(); }}
      />
    {/key}
  {:else if activeWorkspace === 'archives'}
    {#key currentWorkspace?.root}
      <ArchivesPage request={apiFetch} onLibrary={() => selectWorkspaceView('sessions')} onSettings={() => navigateToRoute({ page: 'settings', category: 'remote' })} onChanged={() => { void refreshInventory(true); }} />
    {/key}
  {:else if activeWorkspace === 'settings'}
    <SettingsPage bind:category={settingsCategory} on:categorychange={(event) => navigateToRoute({ page: 'settings', category: event.detail })} let:visibleIDs>
      {#if visibleIDs.includes('density')}
        <SettingsPanel id="density">
          <div class="settings-preference-row">
            <div><strong>列表与阅读布局</strong><p>紧凑显示更多会话，阅读模式给正文更多空间。</p></div>
            <DensityControl value={density} on:change={(event) => setDensity(event.detail)} />
          </div>
          {#if densityStorageError}<p class="settings-save-warning" role="status">当前设置已生效，但浏览器禁止保存；刷新后可能恢复默认。</p>{/if}
        </SettingsPanel>
      {/if}
      {#if visibleIDs.includes('workspaces')}
        <SettingsPanel id="workspaces">
	      <CodexDiscovery candidates={workspaceCandidates} loading={workspaceDiscoveryLoading} busy={workspaceLoading} persisted={workspacePersisted} registryPath={workspaceRegistryPath} serviceAddress={workspaceServiceAddress} message={workspaceDiscoveryMessage} on:refresh={loadWorkspaceDiscovery} on:select={(event) => useDiscoveredWorkspace(event.detail)} />
	      <div class="workspace-switcher">
	        <label class="section-label" for="workspace-select">当前工作区</label>
	        <select id="workspace-select" value={currentWorkspace?.id ?? ''} on:change={(event) => selectWorkspace((event.currentTarget as HTMLSelectElement).value)} disabled={workspaceLoading || workspacePersisted !== true || workspaces.length === 0}>
	          {#if workspaces.length === 0}<option value="">暂无已注册工作区</option>{/if}
	          {#each workspaces as workspace (workspace.id)}
	            <option value={workspace.id}>{workspace.name || workspace.root}</option>
	          {/each}
	        </select>
	        {#if currentWorkspace}<div class="workspace-switcher-path" title={currentWorkspace.root}>{currentWorkspace.root}</div>{/if}
	        {#if workspaces.length > 1}
	          <div class="workspace-remove-list">
	            {#each workspaces.filter((workspace) => workspace.id !== currentWorkspace?.id) as workspace (workspace.id)}
	              <button class="workspace-remove" on:click={() => removeWorkspace(workspace.id)} disabled={workspaceLoading}>移除“{workspace.name || workspace.root}”</button>
	            {/each}
	          </div>
	        {/if}
	      </div>
      <form class="workspace-add" on:submit|preventDefault={() => addWorkspace()}>
	        <div class="section-label">手动添加目录</div>
	        <input bind:value={workspaceName} maxlength="120" placeholder="名称（可选）" aria-label="工作区名称" disabled={workspaceLoading || workspacePersisted !== true} />
	        <input bind:value={workspaceRoot} required placeholder="服务所在设备的源目录绝对路径" aria-label="工作区源根目录" disabled={workspaceLoading || workspacePersisted !== true} />
	        <input bind:value={workspaceArchiveDB} placeholder="独立 archive.db（可选）" aria-label="独立 archive.db 路径" disabled={workspaceLoading || workspacePersisted !== true} />
	        <button class="secondary-button" type="submit" disabled={workspaceLoading || workspacePersisted !== true || !workspaceRoot.trim()}>{workspaceLoading ? '处理中…' : '添加并读取'}</button>
	      </form>
	      {#if workspaceMessage}<div class="workspace-message" role="status">{workspaceMessage}</div>{/if}
        </SettingsPanel>
      {/if}
      {#if visibleIDs.includes('copy')}
        <SettingsPanel id="copy">
      <div class="copy-panel">
        <div class="section-label">制作稳定副本</div>
        <div class="copy-note">先读取并校验当前 .codex，再由你确认发布到外部目录。</div>
        {#if persistedCopyTasks.length}
          <div class="copy-saved-plans">
            <small>已保存的待确认计划</small>
            {#each persistedCopyTasks.slice(0, 4) as savedPlan}
              <button class="copy-saved-plan" on:click={() => resumeCopyPlan(savedPlan)}>
                <span>{savedPlan.destination_root || savedPlan.plan?.destination_root || '外部副本'}</span>
                <small>{savedPlan.plan?.file_count ?? 0} 个文件 · {formatBytes(savedPlan.plan?.total_bytes ?? 0)}</small>
              </button>
            {/each}
          </div>
        {/if}
        <input bind:value={copyDestination} placeholder={copyDefaultDestination() || '副本目标目录（必须位于 .codex 外）'} aria-label="稳定副本目标目录" />
        <label class="copy-exclude-field">
          <span>副本排除路径（每行一个相对路径）</span>
          <textarea bind:value={copyExcludePathsText} rows="3" aria-label="副本排除路径" placeholder="例如：auth.json&#10;tmp&#10;具体路径相对于 .codex 根目录"></textarea>
        </label>
        <div class="copy-note">默认不添加自定义排除项。可按需填写 auth.json 等敏感文件或目录；目录会递归排除，且只影响副本，源 .codex 不会被修改。</div>
        <label class="copy-risk-option"><input type="checkbox" bind:checked={copyAllowUnavailableProcessProbe} />无法探测打开文件进程时仍继续（风险较高）</label>
        <div class="copy-actions">
          <button class="secondary-button" on:click={startCopyPrepare} disabled={copyLoading || !report}>{copyLoading && copyTask?.kind === 'copy_prepare' ? '读取中…' : '生成只读计划'}</button>
          {#if copyTask?.kind === 'copy_prepare' && copyTask.status === 'completed'}
            <button class="copy-confirm" on:click={executeCopy} disabled={copyLoading}>确认制作副本</button>
          {/if}
          {#if copyTask?.result?.published}
            <button class="copy-verify" on:click={verifyCopy} disabled={copyLoading}>验证副本</button>
          {/if}
        </div>
        {#if copyTask}
          <div class:copy-task-error={copyTask.status === 'failed'} class="copy-task">
            <div class="copy-task-head"><strong>{copyTask.status === 'completed' ? '已完成' : copyTask.status === 'failed' ? '已阻断' : copyTask.status === 'cancelled' ? '已取消' : '处理中'}</strong><span>{copyTask.phase || copyTask.kind}</span></div>
            {#if copyTask.files_total || copyTask.bytes_total}
              <div class="copy-progress"><i style={`width: ${Math.min(100, Math.max(0, (copyTask.bytes_done || 0) / Math.max(1, copyTask.bytes_total || 1) * 100))}%`}></i></div>
              <small>{copyTask.files_done || 0} / {copyTask.files_total || 0} 个文件 · {formatBytes(copyTask.bytes_done || 0)} / {formatBytes(copyTask.bytes_total || 0)}</small>
            {/if}
            {#if copyTask.plan}<small>源清单摘要：{copyTask.plan.source_manifest_sha256.slice(0, 16)}… · {copyTask.plan.file_count} 个文件</small>{/if}
            {#if copyTask.plan?.marker}<small>兼容身份：{copyTask.plan.marker.adapter_id || '未绑定'}{copyTask.plan.marker.schema_sha256 ? ` · schema ${copyTask.plan.marker.schema_sha256.slice(0, 12)}…` : ''}</small>{/if}
            {#if copyTask.plan?.activity}
              <small>活动探测：{copyTask.plan.activity.process_probe_available === false ? '进程探测不可用' : '进程探测可用'} · {copyTask.plan.activity.lock_probe_available === false ? '锁目录不可读' : `writer lock ${copyTask.plan.activity.lock_file_count ?? 0} 个`}</small>
              {#if copyTask.plan.activity.lock_ids?.length}
                <details class="copy-excluded">
                  <summary>活动锁 ID（{copyTask.plan.activity.lock_ids.length}）</summary>
                  <div class="copy-excluded-list">
                    {#each copyTask.plan.activity.lock_ids as lockID}<code>{lockID}</code>{/each}
                  </div>
                </details>
              {/if}
            {/if}
            {#if copyTask.plan?.excluded_paths?.length}
              <details class="copy-excluded">
                <summary>副本排除项（{copyTask.plan.excluded_paths.length}）</summary>
                <div class="copy-excluded-list">
                  {#each copyTask.plan.excluded_paths.slice(0, 64) as path}<code>{path}</code>{/each}
                </div>
                {#if copyTask.plan.excluded_paths.length > 64}<small>仅显示前 64 项，其余路径仍保存在副本计划中。</small>{/if}
              </details>
            {/if}
            {#if copyTask.plan?.warnings?.length}{#each copyTask.plan.warnings as warning}<small class="copy-error-text">{warning}</small>{/each}{/if}
            {#if copyTask.error}<small class="copy-error-text">{copyTask.error}</small>{/if}
          </div>
        {/if}
        {#if copyMessage}<div class="copy-note">{copyMessage}</div>{/if}
      </div>

        </SettingsPanel>
      {/if}
      {#if visibleIDs.includes('trash')}
        <SettingsPanel id="trash">
      <div class="trash-detection-panel">
        <div class="recovery-heading"><div class="section-label">系统回收站</div><button class="recovery-refresh" on:click={loadTrashDetection} disabled={trashDetectionLoading || trashSettingsLoading}>{trashDetectionLoading ? '检测中…' : '重新检测'}</button></div>
        {#if trashDetection}
          <div class="stat-line"><span>平台</span><strong>{trashDetection.os} / {trashDetection.architecture}</strong></div>
          <div class="stat-line"><span>检测结果</span><span class={`status-pill ${trashDetection.supported ? 'stable' : 'unsupported'}`}>{trashDetection.supported ? (trashDetection.configured ? '自定义可用' : '候选可用') : '不可用'}</span></div>
          <div class="workspace-switcher-path" title={trashDetection.candidate_path || '未发现候选路径'}>{trashDetection.candidate_path || '未发现候选路径'}</div>
          {#if trashDetection.configured && trashDetection.default_path}<small class="copy-note">系统检测候选：{trashDetection.default_path}</small>{/if}
          <small class="copy-note">{trashDetection.reason}</small>
          <small class="copy-note">只读检测：{trashDetection.exists ? '目录存在' : '目录不存在'} · {trashDetection.is_directory ? '目录格式有效' : '不是目录'} · {trashDetection.writable ? '权限显示可写' : '权限未显示可写'}</small>
          <button class="secondary-button" type="button" on:click={() => openDestinationCheck()} disabled={destinationBusy}>测试并确认位置</button>
          {#if trashDetection.os !== 'windows'}
            <div class="trash-settings-form">
              <label class="section-label" for="trash-custom-path">手动指定回收站目录</label>
              <input id="trash-custom-path" bind:value={trashCustomPath} maxlength="32768" placeholder={trashDetection.default_path || '输入现存目录的绝对路径'} aria-label="自定义回收站目录" />
              <div class="trash-settings-buttons">
                <button class="secondary-button" type="button" on:click={() => saveTrashSettings()} disabled={trashSettingsLoading}>{trashSettingsLoading ? '保存中…' : '保存路径'}</button>
                <button class="recovery-refresh" type="button" on:click={() => saveTrashSettings('')} disabled={trashSettingsLoading}>恢复默认</button>
              </div>
              <small class="copy-note">Linux 路径应指向 Trash/files 目录；设置只接受现存、权限位显示可写且不在 .codex 内的目录。</small>
            </div>
          {:else}
            <small class="copy-note">Windows 操作由系统 Shell 管理，不能通过目录路径覆盖。</small>
          {/if}
          {#if trashSettingsMessage}<div class="trash-settings-message" class:trash-settings-error={trashSettingsError}>{trashSettingsMessage}</div>{/if}
        {:else if trashDetectionLoading}
          <div class="empty-note compact-note">正在读取本机回收站状态…</div>
        {:else}
          <div class="github-note">{trashDetectionMessage || '回收站状态尚未读取'}</div>
        {/if}
      </div>

        </SettingsPanel>
      {/if}
      {#if visibleIDs.includes('recovery')}
        <SettingsPanel id="recovery">
      <div class="recovery-panel">
        <div class="recovery-heading"><div class="section-label">待恢复操作{#if recoveryItems.length} · {recoveryItems.length}{/if}</div><button class="recovery-refresh" on:click={loadRecoveryItems} disabled={recoveryLoading}>{recoveryLoading ? '读取中…' : '刷新'}</button></div>
        {#if recoveryLoading && !recoveryItems.length}
          <div class="empty-note compact-note">正在读取 journal…</div>
        {:else if !recoveryAvailable && !recoveryItems.length}
          <div class="github-note">当前 daemon 未配置独立恢复 journal。</div>
        {:else if recoveryItems.length === 0}
          <div class="empty-note compact-note">没有待人工复核的生命周期操作。</div>
        {:else}
          <div class="recovery-list">
            {#each recoveryItems as item (item.operation_id)}
              <div class="recovery-row">
                <div class="recovery-row-head"><strong>{item.conversation_id || item.operation_id}</strong><span class="status-pill recovery-phase">{recoveryPhaseLabels[item.phase] || item.phase}</span></div>
                <small>{formatTime(item.updated_at)} · {item.asset_count} 个资产 · {formatBytes(item.bytes)} · {item.receipt_count} 个回收站回执</small>
                <div class="recovery-disposition" class:recovery-unknown={item.external_state_unknown}>{recoveryDispositionLabels[item.disposition] || item.disposition}</div>
                {#if item.error_message}<small class="recovery-error">{item.error_code ? `${item.error_code}：` : ''}{item.error_message}</small>{/if}
                {#if item.recovery_note}<small class="recovery-note">{item.recovery_note}</small>{/if}
                {#if item.reason}<small class="recovery-reason">判定依据：{item.reason}</small>{/if}
                {#if item.can_rollback}
                  <button class="recovery-button" on:click={() => recoverOperation(item)} disabled={recoveryExecuting !== ''}>{recoveryExecuting === item.operation_id ? '恢复中…' : '恢复并回滚回收站资产'}</button>
                {:else if item.external_state_unknown}
                  <small class="recovery-action-blocked">外部状态无法确认，已禁止自动回滚，请检查 staging、归档和 journal。</small>
                {:else if item.can_retry}
                  <small class="recovery-action-hint">可重新扫描并生成新的执行计划；当前入口不会直接重试。</small>
                {:else}
                  <small class="recovery-action-blocked">当前状态需要人工复核，未提供自动操作。</small>
                {/if}
              </div>
            {/each}
          </div>
        {/if}
        {#if recoveryErrors.length}<div class="recovery-error">{recoveryErrors.join('；')}</div>{/if}
        {#if recoveryMessage}<div class="recovery-message">{recoveryMessage}</div>{/if}
      </div>


        </SettingsPanel>
      {/if}
      {#if visibleIDs.includes('restore')}
        <SettingsPanel id="restore">
      <div class="local-restore-panel">
        <div class="recovery-heading"><div class="section-label">本地归档恢复{#if localRestoreRecoveryItems.length} · {localRestoreRecoveryItems.length}{/if}</div><button class="recovery-refresh" on:click={refreshLocalRestoreSources} disabled={localRestoreRecoveryLoading || localArchivesLoading}>{localRestoreRecoveryLoading || localArchivesLoading ? '读取中…' : '刷新'}</button></div>
        {#if localArchivesAvailable && localArchives.length}
          <select bind:value={localRestoreArchivePath} aria-label="从本地归档清单选择归档">
            <option value="">从 ArchiveRoot 选择归档…</option>
            {#each localArchives as item (item.path)}
              <option value={item.path}>{item.operation_id} · {item.asset_count} 个文件{item.encrypted ? ' · 已加密' : ''}{item.staging ? ' · staging' : ''}</option>
            {/each}
          </select>
        {:else if localArchivesLoading}
          <div class="empty-note compact-note">正在读取本地归档清单…</div>
        {:else if !localArchivesAvailable}
          <div class="github-note">尚未配置独立 ArchiveRoot，可手动输入归档目录。</div>
        {:else}
          <div class="empty-note compact-note">ArchiveRoot 内暂无可识别归档。</div>
        {/if}
        <input bind:value={localRestoreArchivePath} placeholder="归档目录绝对路径" aria-label="本地归档目录绝对路径" />
        <input bind:value={localRestoreTargetRoot} placeholder="恢复目标目录（可选，来源 journal 可省略）" aria-label="恢复目标目录" />
        <input bind:value={localRestoreSourceOperationID} placeholder="来源删除 operation ID（可选）" aria-label="来源删除 operation ID" />
        <input bind:value={localRestorePassword} type="password" placeholder="加密归档密码（仅本次操作）" aria-label="加密归档密码" />
        <div class="local-restore-actions">
          <button class="secondary-button" on:click={previewLocalArchiveRestore} disabled={localRestoreLoading || !localRestoreArchivePath.trim()}>{localRestoreLoading ? '处理中…' : '预览恢复'}</button>
          {#if localRestorePlan?.preview.ready}<button class="copy-confirm" on:click={executeLocalArchiveRestore} disabled={localRestoreLoading}>确认恢复</button>{/if}
        </div>
        {#if localRestorePlan}
          <div class:local-restore-blocked={!localRestorePlan.preview.ready} class="local-restore-preview">
            <strong>{localRestorePlan.preview.ready ? '预览通过' : '存在目标冲突'}</strong>
            <span>{localRestorePlan.preview.files.length} 个文件 · {localRestorePlan.preview.encrypted ? '已加密' : '未加密'} · {localRestorePlan.control_plane_bound ? '控制面已绑定' : '控制面需来源 journal'}</span>
            {#if localRestorePlan.preview.conflicts?.length}<small>{localRestorePlan.preview.conflicts.join('；')}</small>{/if}
            <small>恢复操作：{localRestorePlan.restore_operation_id}</small>
          </div>
        {/if}
        {#if localRestoreRecoveryItems.length}
          <div class="local-restore-recovery-list">
            {#each localRestoreRecoveryItems as item (item.restore_operation_id)}
              <div class="recovery-row">
                <div class="recovery-row-head"><strong>{item.restore_operation_id}</strong><span class="status-pill recovery-phase">{item.phase}</span></div>
                <small>{item.asset_count} 个文件 · {formatTime(item.updated_at)}</small>
                {#if item.error_message}<small class="recovery-error">{item.error_code ? `${item.error_code}：` : ''}{item.error_message}</small>{/if}
                <button class="recovery-button" on:click={() => recoverLocalArchive(item)} disabled={localRestoreRecoveryExecuting !== ''}>{localRestoreRecoveryExecuting === item.restore_operation_id ? '续接中…' : '续接恢复'}</button>
              </div>
            {/each}
          </div>
        {:else if localRestoreRecoveryAvailable && !localRestoreRecoveryLoading}
          <div class="empty-note compact-note">没有待续接的归档恢复。</div>
        {/if}
        {#if localArchivesMessage}<div class="recovery-error">{localArchivesMessage}</div>{/if}
        {#if localRestoreMessage}<div class="recovery-message">{localRestoreMessage}</div>{/if}
      </div>

        </SettingsPanel>
      {/if}
      {#if visibleIDs.includes('connection')}
        <SettingsPanel id="connection">
      <GitHubConnection request={apiFetch} on:saved={async () => {
        githubSession = null;
        githubAccount = null;
        githubCreatedRepository = null;
        githubDownloadPlan = null;
        githubDeletePlan = null;
        remoteRestorePlan = null;
        await loadRemoteSetup();
      }}>
      {#if remoteProvider?.id === 'github' || githubStatus?.configured}
      <div class="github-binding">
        <ol class="github-steps" aria-label="GitHub 归档设置进度">
          <li class:complete={Boolean(githubAccount)}><span>{#if githubAccount}<Check size={13} />{:else}1{/if}</span>连接账户</li>
          <li class:complete={Boolean(githubArchiveTarget)}><span>{#if githubArchiveTarget}<Check size={13} />{:else}2{/if}</span>选择私有仓库</li>
        </ol>
        {#if !githubStatus}
          <div class="empty-note compact-note">正在读取绑定状态…</div>
        {:else}
          <div class="github-status-line"><KeyRound size={18} /><span>{githubAccount ? `已连接 @${githubAccount.login}` : githubStatus.credential_available ? '凭证已保存 · 账户待核验' : '连接 GitHub，将归档保存到自己的私有仓库'}</span></div>
          {#if githubSession?.status === 'pending'}
            <div class="github-session">
              <span>在 GitHub 中输入授权码</span><code class="github-user-code">{githubSession.user_code}</code>
              <button class="secondary-button" on:click={copyGitHubCodeAndOpen}><Copy size={14} />复制授权码并打开 GitHub<ExternalLink size={13} /></button>
              <div class="github-session-status" role="status"><LoaderCircle size={13} class="connection-spinner" />等待授权，完成后自动连接</div>
              <button class="github-verify" on:click={loadGitHubStatus} disabled={githubStatusLoading}>已在 GitHub 授权？检查状态</button>
              {#if githubCodeMessage}<span role="status">{githubCodeMessage}</span>{/if}
              <a href={githubSession.verification_uri_complete || githubSession.verification_uri} target="_blank" rel="noreferrer">打开授权页面</a>
            </div>
          {:else if !githubStatus.credential_available}
            <div class="github-connect-actions">
              <button class="secondary-button" on:click={startGitHubBinding} disabled={githubLoading || !(githubStatus.device_flow_available ?? githubStatus.configured)}><KeyRound size={15} />{githubLoading ? '连接中…' : '连接 GitHub'}</button>
              {#if githubStatus.cli_available}<button class="secondary-button" on:click={bindGitHubCLI} disabled={githubLoading}>使用已登录的 GitHub CLI</button>{/if}
            </div>
            {#if !(githubStatus.device_flow_available ?? githubStatus.configured)}<div class="github-note">本版本尚未配置项目登录应用。{githubStatus.cli_available ? '可直接使用服务所在设备的 GitHub CLI 登录。' : '可由维护者配置登录应用，或在高级设置中使用自建应用。'}</div>{/if}
            {#if githubStatus.cli_available}<small class="github-connection-hint">点击后读取服务所在设备的现有 CLI 登录，并保存到系统凭证库。</small>{/if}
          {/if}
          {#if githubStatus.credential_available}
            <details class="github-account-actions"><summary>账户选项</summary><div class="github-connect-actions">
              {#if githubStatus.device_flow_available ?? githubStatus.configured}<button class="secondary-button" on:click={startGitHubBinding} disabled={githubLoading}>重新连接 GitHub</button>{/if}
              {#if githubStatus.cli_available}<button class="secondary-button" on:click={bindGitHubCLI} disabled={githubLoading}>使用 GitHub CLI 账户</button>{/if}
              <button class="github-revoke" on:click={revokeGitHubBinding} disabled={githubLoading}>解除本地绑定</button>
            </div><small class="github-connection-hint">解除仅移除本项目保存的凭证；GitHub CLI 登录与远端仓库保留。</small></details>
          {/if}
          {#if githubAccountMessage}<div class="github-error" role="alert">{githubAccountMessage}<button class="github-verify" on:click={loadGitHubAccount} disabled={githubAccountLoading}>重新核验</button></div>{/if}
          {#if githubSession?.error}<div class="github-error" role="alert">{githubSession.error}</div>{/if}
        {/if}
        {#if githubMessage}<div class="github-error">{githubMessage}</div>{/if}
      </div>
      {:else}
        <div class="github-note">当前服务使用其他远端存储。</div>
      {/if}
      </GitHubConnection>

        </SettingsPanel>
      {/if}
      {#if visibleIDs.includes('repositories')}
        <SettingsPanel id="repositories">
      {#if remoteProvider?.id === 'github'}
      <div class="github-repositories">
        <div class="section-label">GitHub 仓库</div>
        {#if githubRepositoriesLoading}
          <div class="empty-note compact-note">正在读取仓库…</div>
        {:else if !githubRepositoriesConfigured}
          <div class="github-note">连接账户后可选择或创建私有仓库</div>
        {:else if !githubRepositoriesCredentialAvailable}
          <div class="github-note">连接账户后可选择或创建私有仓库</div>
        {:else}
          {#if githubArchiveTargetLoading}
            <div class="empty-note compact-note">正在读取当前归档仓库…</div>
          {:else if githubArchiveTarget}
            <div class="github-note">当前归档仓库：<strong>{githubArchiveTarget.full_name}</strong></div>
          {:else}
            <form class="github-repository-create" on:submit|preventDefault={createGitHubRepository}>
              <div class="github-note">归档仓库：<strong>{githubCreatedRepository?.full_name || githubRepositoryName}</strong> · 私有</div>
              <details class="github-repository-options"><summary>修改仓库名称和描述</summary>
                <input bind:value={githubRepositoryName} required maxlength="100" pattern="[A-Za-z0-9._-]+" placeholder="新私有仓库名称" aria-label="新私有仓库名称" disabled={githubRepositoryCreating || Boolean(githubCreatedRepository)} />
                <input bind:value={githubRepositoryDescription} maxlength="200" placeholder="描述（可选）" aria-label="仓库描述" disabled={githubRepositoryCreating || Boolean(githubCreatedRepository)} />
              </details>
              <button class="secondary-button" type="submit" disabled={githubRepositoryCreating || githubArchiveTargetSaving || !githubRepositoryName.trim()}>{githubRepositoryCreating ? '正在准备归档仓库…' : githubCreatedRepository ? '重试使用已创建仓库' : '创建并使用私有归档仓库'}</button>
            </form>
          {/if}
          <details class="github-repository-options">
            <summary>选择已有私有仓库{githubRepositories.length ? `（${githubRepositories.length}）` : ''}</summary>
          {#if githubRepositories.length}
            <div class="github-list-toolbar"><input type="search" bind:value={githubRepositoryQuery} on:input={() => githubRepositoryLimit = 20} placeholder="搜索仓库" aria-label="搜索 GitHub 仓库" /><button class="secondary-button" on:click={loadGitHubRepositories} disabled={githubRepositoriesLoading}>刷新仓库</button></div>
            <div class="github-repository-list">
              {#each githubRepositoryMatches.slice(0, githubRepositoryLimit) as repository}
                <div class="github-repository-row">
                  <strong>{repository.full_name}</strong>
                  <span>{repository.private ? '私有' : '公开'} · {repository.default_branch || '默认分支未知'}</span>
                  <button class="github-verify" on:click={() => selectGitHubArchiveTarget(repository)} disabled={!repository.private || githubArchiveTargetSaving || githubArchiveTarget?.full_name === repository.full_name}>{githubArchiveTarget?.full_name === repository.full_name ? '当前目标' : !repository.private ? '仅支持私有仓库' : '选择归档'}</button>
                </div>
              {/each}
            </div>
            {#if !githubRepositoryMatches.length}<div class="github-note">没有匹配的仓库。</div>{/if}
            {#if githubRepositoryMatches.length > githubRepositoryLimit}<button class="secondary-button" on:click={() => githubRepositoryLimit += 20}>显示更多（还有 {githubRepositoryMatches.length - githubRepositoryLimit} 个）</button>{/if}
          {:else}
            <div class="github-note">尚无可用仓库，可创建一个私有仓库。<button class="github-verify" on:click={loadGitHubRepositories}>刷新</button></div>
          {/if}
          </details>
        {/if}
        {#if githubRepositoryMessage}<div class="github-note">{githubRepositoryMessage}</div>{/if}
      </div>
      {:else}
        <div class="settings-unavailable">当前远端服务未使用 GitHub 仓库。</div>
      {/if}

        </SettingsPanel>
      {/if}
      {#if visibleIDs.includes('remoteArchives')}
        <SettingsPanel id="remoteArchives">
      <div class="github-archives">
        <div class="github-list-toolbar"><div class="section-label">{remoteProvider?.name || '远端'}归档清单</div><button class="github-verify" on:click={loadGitHubArchives} disabled={githubArchivesLoading || !remoteProviderConfigured}>刷新清单</button></div>
        {#if githubArchivesLoading}
          <div class="empty-note compact-note">正在读取远端清单…</div>
        {:else if !remoteProviderConfigured}
          <div class="github-note">请先配置远端连接。</div>
        {:else if !githubArchivesCredentialAvailable}
          <div class="github-note">完成远端认证后可读取远端清单</div>
        {:else if remoteTargetRequired && (!githubArchiveTarget || !githubArchivesConfigured)}
          <div class="github-note">请先选择远端归档目标</div>
        {:else if githubArchives.length === 0}
          <div class="github-note">远端没有可识别的归档批次</div>
        {:else}
          <div class="github-archive-list">
            {#each githubArchives.slice(0, githubArchiveLimit) as remoteArchive}
              <div class="github-archive-row">
                <div><strong>{remoteArchive.archive_id}</strong><span>{remoteArchive.file_count} 文件 · {remoteArchive.chunk_count} 分片</span></div>
                <div class="github-archive-actions">
                  <button class="github-verify" on:click={() => verifyGitHubArchive(remoteArchive.archive_id)} disabled={githubArchiveVerifying === remoteArchive.archive_id || githubArchiveDownloading === remoteArchive.archive_id}>{githubArchiveVerifying === remoteArchive.archive_id ? '校验中…' : '校验'}</button>
                  <button class="github-verify" on:click={() => previewGitHubArchiveDownload(remoteArchive.archive_id)} disabled={githubArchiveDownloading === remoteArchive.archive_id || githubArchiveRestoring === remoteArchive.archive_id || !githubDownloadDestination.trim()}>{githubArchiveDownloading === remoteArchive.archive_id ? '处理中…' : '下载预览'}</button>
                  <button class="github-verify" on:click={() => previewRemoteArchiveRestore(remoteArchive.archive_id)} disabled={githubArchiveDownloading === remoteArchive.archive_id || githubArchiveRestoring === remoteArchive.archive_id || !remoteRestoreTargetRoot.trim()}>{githubArchiveRestoring === remoteArchive.archive_id ? '处理中…' : '恢复预览'}</button>
                  <button class="github-danger" on:click={() => previewGitHubArchiveDelete(remoteArchive.archive_id)} disabled={githubArchiveDeleting === remoteArchive.archive_id}>{githubArchiveDeleting === remoteArchive.archive_id ? '处理中…' : '删除预览'}</button>
                </div>
              </div>
            {/each}
            {#if githubArchives.length > githubArchiveLimit}<button class="secondary-button" on:click={() => githubArchiveLimit += 20}>显示更多（还有 {githubArchives.length - githubArchiveLimit} 个）</button>{/if}
          </div>
        {/if}
        {#if githubArchiveMessage}<div class="github-note">{githubArchiveMessage}</div>{/if}
        {#if remoteProviderConfigured && githubArchivesCredentialAvailable && (!remoteTargetRequired || githubArchiveTarget)}
          <div class="github-download-form">
            <input bind:value={githubDownloadDestination} placeholder="下载到已配置恢复根目录内的文件夹" aria-label="远端归档下载目录" />
            {#if githubDownloadPlan}
              <div class="github-note">预览：将写入 {githubDownloadPlan.output_directory}，包含 {githubDownloadPlan.archive.file_count} 个文件和 {githubDownloadPlan.archive.chunk_count} 个分片。</div>
              <button class="secondary-button" on:click={executeGitHubArchiveDownload} disabled={githubArchiveDownloading !== ''}>确认下载到隔离目录</button>
            {/if}
            {#if githubDeletePlan}
              <div class="github-danger-note">将删除远端归档 {githubDeletePlan.archive_id} 的 manifest 和全部分片，本地源文件与本地归档不受影响。</div>
              <input bind:value={githubDeleteConfirmation} placeholder={`输入 ${githubDeletePlan.confirmation_required} 以确认`} aria-label="远端归档删除确认" />
              <button class="github-danger-button" on:click={executeGitHubArchiveDelete} disabled={githubArchiveDeleting !== '' || githubDeleteConfirmation !== githubDeletePlan.confirmation_required}>确认删除远端归档</button>
            {/if}
          </div>
          <details class="github-restore-form">
            <summary>恢复远端归档到本机</summary>
            <input bind:value={remoteRestoreTargetRoot} placeholder="本机恢复目标目录（必须是独立目录）" aria-label="远端归档本机恢复目标目录" />
            <input bind:value={remoteRestoreSourceOperationID} placeholder="来源删除 operation ID（可选，用于控制面恢复）" aria-label="远端归档来源 operation ID" />
            <input bind:value={remoteRestorePassword} type="password" placeholder="加密归档密码（仅本次操作）" aria-label="远端归档恢复密码" />
            <div class="github-note">点击某个批次的“恢复预览”后，系统会将已校验归档下载到独立 staging，再展示本地文件冲突。预览阶段会产生本项目归档副本。</div>
            {#if remoteRestorePlan}
              <div class:local-restore-blocked={!remoteRestorePlan.plan.preview.ready} class="local-restore-preview">
                <strong>{remoteRestorePlan.plan.preview.ready ? '远端恢复预览通过' : '远端恢复存在目标冲突'}</strong>
                <span>{remoteRestorePlan.plan.preview.files.length} 个文件 · {remoteRestorePlan.plan.preview.encrypted ? '已加密' : '未加密'} · {remoteRestorePlan.plan.control_plane_bound ? '控制面已绑定' : '控制面需来源 journal'}</span>
                {#if remoteRestorePlan.plan.preview.conflicts?.length}<small>{remoteRestorePlan.plan.preview.conflicts.join('；')}</small>{/if}
                <small>本地 staging：{remoteRestorePlan.archive_path}</small>
                <div class="local-restore-actions"><button class="copy-confirm" on:click={executeRemoteArchiveRestore} disabled={githubArchiveRestoring !== '' || !remoteRestorePlan.plan.preview.ready}>确认恢复</button></div>
              </div>
            {/if}
          </details>
        {/if}
      </div>

        </SettingsPanel>
      {/if}
      {#if visibleIDs.includes('adapter')}
        <SettingsPanel id="adapter">
      {#if report}
        <div class="workspace-card">
          <div class="workspace-icon">⌂</div>
          <div class="workspace-path" title={report.root}>{report.root}</div>
        </div>
        {#if report.adapter}
          <div class="adapter-card">
            <div><span class:adapter-ok={report.adapter.matched} class="adapter-dot"></span><strong>{report.adapter.descriptor?.id || '通用只读'}</strong></div>
            <span>{report.adapter.identity?.version || '版本未知'} · {report.adapter.matched ? '已匹配' : '只读降级'}{report.adapter.identity?.schema_fingerprints?.length ? ` · ${report.adapter.identity.schema_fingerprints.length} 个 schema` : ''}</span>
          </div>
        {/if}
      {:else}
        <div class="empty-note">尚未连接 daemon</div>
      {/if}

        </SettingsPanel>
      {/if}
      {#if visibleIDs.includes('platform')}
        <SettingsPanel id="platform">
      <div class="platform-capabilities-panel">
        <div class="recovery-heading"><div class="section-label">平台能力边界</div><button class="recovery-refresh" on:click={loadPlatformCapabilities} disabled={platformCapabilitiesLoading}>{platformCapabilitiesLoading ? '读取中…' : '重新读取'}</button></div>
        {#if platformCapabilities}
          <div class="stat-line"><span>运行时</span><strong>{platformCapabilities.os} / {platformCapabilities.architecture}</strong></div>
          <div class="platform-capability-list">
            {#each platformCapabilities.capabilities as capability (capability.id)}
              <div class="platform-capability-row">
                <div><strong>{capability.name}</strong><span>{capability.id}</span></div>
                <span class={`status-pill ${capability.status === 'available' ? 'stable' : capability.status === 'unavailable' ? 'unsupported' : 'active_writing'}`}>{platformCapabilityLabels[capability.status] || capability.status}</span>
                <small>{capability.reason}</small>
              </div>
            {/each}
          </div>
          <small class="copy-note">以上均为只读探针；有限或未实测能力会让稳定副本和生命周期操作保持保守阻断。</small>
        {:else if platformCapabilitiesLoading}
          <div class="empty-note compact-note">正在读取本机平台能力…</div>
        {:else}
          <div class="github-note">{platformCapabilitiesMessage || '平台能力尚未读取'}</div>
        {/if}
      </div>

        </SettingsPanel>
      {/if}
      {#if visibleIDs.includes('updates')}
        <SettingsPanel id="updates">
      {#if updateStatus}
      <div class="update-panel">
        <div class="section-label">离线更新</div>
        <div class="update-status-line"><span>当前版本</span><strong>{updateStatus.state.active_version || '未激活'}</strong></div>
        {#if updateStatus.state.previous_version}<div class="update-status-line"><span>可回滚版本</span><strong>{updateStatus.state.previous_version}</strong></div>{/if}
        <div class="update-status-line"><span>清单签名</span><strong>{updateStatus.signature_required ? `强制 · ${updateStatus.trusted_key_count} 个信任根` : '可用但未强制'}</strong></div>
        {#if updateStatus.recovery_required}<div class="update-warning">{updateStatus.recovery_message || '检测到未完成更新 journal，请先人工检查。'}</div>{/if}
        {#if updatePreflight?.blocked}<div class="update-warning">更新安全预检已阻断：{updatePreflight.migration?.status || (updatePreflight.adapter?.errors || []).join('；') || '请先处理迁移或兼容 adapter 问题。'}</div>{:else if updatePreflight}<small class="copy-note">更新安全预检：通过 · archive.db {updatePreflight.migration?.status || 'ready'}</small>{/if}
        {#if updateStatus.online_configured}
          <div class="update-online">
            <div class="update-online-head"><span>在线 feed</span><small>{updateStatus.online_feed_url || '已配置'}</small></div>
            <button class="secondary-button" on:click={checkOnlineUpdate} disabled={updateLoading}>{updateLoading ? '检查中…' : '检查在线更新'}</button>
            {#if onlineUpdate?.available && onlineUpdate.release}
              <div class="update-online-release"><strong>{onlineUpdate.release.version}</strong><span>{onlineUpdate.release.os}/{onlineUpdate.release.arch} · {formatBytes(onlineUpdate.release.size_bytes)}</span><button class="copy-confirm" on:click={prepareOnlineUpdate} disabled={updateLoading}>下载并预检</button></div>
            {:else if onlineUpdate}
              <small class="copy-note">检查于 {formatTime(onlineUpdate.checked_at)}，当前没有更高版本。</small>
            {/if}
          </div>
        {/if}
        <input bind:value={updateBundleRoot} placeholder="离线包目录或 ZIP/TAR 文件绝对路径" aria-label="离线包目录或 ZIP/TAR 文件" />
        <button class="secondary-button" on:click={prepareUpdate} disabled={updateLoading || !updateBundleRoot.trim()}>{updateLoading ? '处理中…' : '预检离线包'}</button>
        {#if updatePlan}
          <div class="update-plan"><strong>待确认：{updatePlan.version}</strong><span>{updatePlan.file_count} 个文件 · {formatBytes(updatePlan.total_bytes)}</span><small>计划 {updatePlan.id}</small><button class="copy-confirm" on:click={activateUpdate} disabled={updateLoading || updatePreflight?.blocked}>确认切换版本</button></div>
        {/if}
        {#if updateStatus.state.previous_version}<button class="update-rollback" on:click={rollbackUpdate} disabled={updateLoading}>回滚到 {updateStatus.state.previous_version}</button>{/if}
        <small class="copy-note">更新只切换独立安装根的版本指针，不修改 Codex 数据；切换后需要按部署方式重启 daemon。</small>
        {#if updateMessage}<div class="update-message">{updateMessage}</div>{/if}
      </div>
      {:else}
        <div class="settings-unavailable">当前服务尚未提供更新配置。</div>
      {/if}

        </SettingsPanel>
      {/if}
    </SettingsPage>
  {:else}
  <main class="workspace session-library" aria-label="会话库" class:compact-pane-workspace={compactPane === 'workspace'} class:compact-pane-sessions={compactPane === 'sessions'} class:compact-pane-detail={compactPane === 'detail'}>
    <aside class="sidebar panel library-sidebar" aria-label="项目与视图">
      <div class="panel-heading"><strong>项目与视图</strong><button type="button" class="library-icon-button library-sidebar-back" aria-label="返回会话列表" on:click={() => compactPane = 'sessions'}><X aria-hidden="true" /></button></div>
      <div class="library-sidebar-scroll">
        <button type="button" class="filter-row library-all-sessions" class:active={!activeConversationFilterCount && !searchExecutedQuery} on:click={clearConversationFilters}><span><MessagesSquare aria-hidden="true" />全部会话</span><strong>{formatNumber(report?.analytics?.conversation_count ?? conversationTotal)}</strong></button>
        <details class="library-projects" open>
          <summary><span>项目</span><small>{projectFilters.length}</small></summary>
          {#if projectFilters.length > 6}<input class="library-project-search" type="search" bind:value={projectSearch} aria-label="查找项目" placeholder="查找项目…" />{/if}
          <div class="library-project-list">
            {#each visibleProjectFilters.slice(0, 40) as project (project.name)}
              <button type="button" class="filter-row library-project-row" class:active={cwdFilter === project.name} aria-pressed={cwdFilter === project.name} title={project.name} on:click={() => drillIntoAnalytics('project', project.name)}><span><FolderOpen aria-hidden="true" /><span>{projectLabel(project.name)}</span></span><strong>{formatNumber(project.count)}</strong></button>
            {:else}<p class="library-sidebar-empty">{projectSearch ? '没有匹配的项目' : '尚未发现项目'}</p>{/each}
          </div>
          {#if visibleProjectFilters.length > 40}<small class="library-sidebar-hint">显示前 40 项，输入名称继续查找</small>{/if}
        </details>
        <div class="library-sidebar-section">
          <label class="section-label" for="library-status">对话状态</label>
          <select id="library-status" value={statusFilter} on:change={(event) => setStatusFilter(event.currentTarget.value)}>
            <option value="全部状态">全部状态</option>
            {#each Object.entries(statusLabels) as [status, label]}{#if statusCounts[status]}<option value={status}>{label}（{formatNumber(statusCounts[status])}）</option>{/if}{/each}
          </select>
        </div>
        <div class="library-sidebar-section">
          <div class="section-label">保存的视图</div>
          {#if savedFilters.length}
            <div class="saved-filter-list">
              {#each savedFilters as savedFilter (savedFilter.id)}
                <div class="saved-filter-row"><button type="button" class="saved-filter-apply" on:click={() => applySavedFilter(savedFilter)} title={savedFilterDescription(savedFilter)}><strong>{savedFilter.name}</strong><small>{savedFilterDescription(savedFilter)}</small></button><button type="button" class="saved-filter-delete library-icon-button" aria-label={`删除保存的筛选 ${savedFilter.name}`} on:click={() => deleteSavedFilter(savedFilter.id)}><X aria-hidden="true" /></button></div>
              {/each}
            </div>
          {:else}<p class="library-sidebar-empty">常用筛选可以保存在这里</p>{/if}
          <details class="library-save-view"><summary>保存当前筛选</summary><form class="saved-filter-form" on:submit|preventDefault={saveCurrentFilter}><input bind:value={savedFilterName} maxlength="80" placeholder="视图名称" aria-label="保存筛选名称" /><button class="secondary-button" type="submit">保存</button></form></details>
        </div>
      </div>
      <footer class="library-source"><FolderOpen aria-hidden="true" /><div><strong>{currentWorkspace?.name || '当前工作区'}</strong><span title={report?.root}>{report?.root ? projectLabel(report.root) : '等待连接'}</span></div></footer>
    </aside>

    <section class="asset-column panel">
      <header class="library-list-header">
        <div class="library-list-heading">
          <button type="button" class="library-icon-button library-show-sidebar" aria-label="打开项目与视图" on:click={() => compactPane = 'workspace'}><PanelLeft aria-hidden="true" /></button>
          <div><h2>会话库 <span>{formatNumber(searchIsApplied ? displayedConversations.length : conversationTotal)}</span></h2><p>{conversationsLoading && !conversations.length ? '正在读取对话列表…' : searchIsApplied ? `已载入命中 ${formatNumber(displayedConversations.length)} 个会话 · ${formatNumber(searchHits.length)} 处正文` : `最近更新 · 已载入 ${formatNumber(conversations.length)} / ${formatNumber(conversationTotal)}`}</p></div>
          <button type="button" class="secondary-button library-filter-button" aria-expanded={conversationFiltersOpen} on:click={openConversationFilters}><ListFilter aria-hidden="true" />筛选{#if activeConversationFilterCount}<span>{activeConversationFilterCount}</span>{/if}</button>
        </div>
        <div class="library-read-status" class:library-stale={report?.stale || report?.snapshot_status === 'stale'} role="status"><span class="status-dot"></span><span>{liveModeLabel}</span>{#if report?.stale || report?.snapshot_status === 'stale'}<strong>数据有变化</strong><button type="button" on:click={() => refreshInventory()} disabled={loading}>{loading ? '刷新中…' : '刷新'}</button>{:else}<span class="library-status-time">{loading ? '正在读取…' : report ? '快照已载入' : '正在连接…'}</span>{/if}</div>
        {#if searchLimited}<p class="library-sidebar-empty" role="status">本次仅检查前 500 条候选记录。可细化关键词，或在单个会话内继续搜索。</p>{/if}
      </header>
      {#if activeFilterLabels.length}
        <div class="library-filter-chips" aria-label="当前筛选条件">
          {#each activeFilterLabels as filter (filter.key)}
            <span class="library-filter-chip"><button type="button" class="library-chip-value" title={`${filter.label}：${filter.value}`} aria-label={`编辑${filter.label}筛选：${filter.value}`} on:click={openConversationFilters}><span>{filter.label}</span><strong>{filter.key === 'cwd' ? projectLabel(filter.value) : filter.value}</strong></button><button type="button" class="library-chip-remove" aria-label={`移除${filter.label}筛选`} on:click={() => removeSessionFilter(filter.key)}><X aria-hidden="true" /></button></span>
          {/each}
          <button type="button" class="filter-clear-link" on:click={clearConversationFilters}>清除全部</button>
        </div>
      {/if}
      <div class="library-selection-bar" class:has-selection={selectedConversationCount > 0}>
        <label title="仅选择当前筛选中已载入的会话"><input type="checkbox" checked={allLoadedConversationsSelected} indeterminate={selectedConversationCount > 0 && !allLoadedConversationsSelected} disabled={!displayedConversations.length || batchOperationLoading} on:change={(event) => event.currentTarget.checked ? selectLoadedConversations() : clearConversationSelection()} aria-label="选择当前已载入会话" /><span>{selectedConversationCount ? `已选 ${selectedConversationCount} 项` : '选择已载入'}</span></label>
        {#if selectedConversationCount}<button type="button" class="library-text-button" on:click={clearConversationSelection}>取消选择</button><button type="button" class="library-text-button library-batch-open" on:click={() => batchPanelOpen = true}>批量处理<ChevronRight aria-hidden="true" /></button>{:else if latestBatchReview?.preview.items.length}<button type="button" class="library-text-button library-batch-open" on:click={restoreLatestBatchSelection}>恢复{latestBatchReview.selection_origin === 'reconstructed_from_report' ? '待核对' : '最近预览的'} {latestBatchReview.preview.items.length} 项<ChevronRight aria-hidden="true" /></button>{:else if summaryTask || batchOperationPreview || batchOperationMessage}<button type="button" class="library-text-button library-batch-open" on:click={() => batchPanelOpen = true}>{summaryTask ? `摘要 ${summaryTask.completed}/${summaryTask.requested}` : '查看处理结果'}<ChevronRight aria-hidden="true" /></button>{/if}
      </div>
      {#if executionResult && !batchPanelOpen}<div class="operation-outcome" role="status"><span>{executionResultLabel(executionResult)}</span><button type="button" class="library-text-button" on:click={() => selectWorkspaceView('pending')}>查看执行记录</button><button type="button" class="library-icon-button" aria-label="关闭处理结果" on:click={() => executionResult = null}><X aria-hidden="true" /></button></div>{/if}
      {#if errorMessage}
        <div class="error-card" role="alert"><strong>暂时无法载入</strong><span>{errorMessage}</span><button class="secondary-button" on:click={() => refreshInventory()} disabled={loading}>{loading ? '重试中…' : '重试'}</button></div>
      {:else if (loading || conversationsLoading) && (!report || conversationsLoading && conversations.length === 0)}
        <div class="loading-state"><div class="loading-state-icon" role="status" aria-label="正在加载"><LoaderCircle aria-hidden="true" /></div><strong>{report && conversationsLoading ? '正在读取历史对话' : '正在读取固定扫描根'}</strong><span>{report && conversationsLoading ? '正在整理会话列表，完成后即可开始阅读。' : '首次扫描可能需要一些时间，页面会逐步载入结果。'}</span><div class="loading-lines"><i></i><i></i><i></i></div></div>
      {:else if searchHasNoResults || searchHasNoVisibleResults}
        <div class="no-results-state search-no-results" aria-live="polite">
          <div class="no-results-icon">⌕</div>
          <strong>{searchHasNoResults ? searchLimited ? '已检查范围内没有命中' : '没有找到相关正文' : '命中会话尚未载入'}</strong>
          <span>{searchHasNoResults ? searchLimited ? '尚有候选记录未检查，请细化关键词或打开单个会话搜索。' : `“${searchExecutedQuery}”没有命中可读取的对话正文。可以修改关键词，或清除搜索查看全部会话。` : `已找到 ${formatNumber(searchHits.length)} 条正文命中，但对应会话不在当前载入页。加载下一页后可继续查看。`}</span>
          {#if searchHasNoVisibleResults && conversationOffset < conversationTotal}<button class="secondary-button" on:click={() => void loadMoreConversations()} disabled={conversationsLoadingMore}>{conversationsLoadingMore ? '正在加载下一页…' : '加载下一页会话'}</button>{:else}<button class="secondary-button" on:click={() => void clearSearch()}>清除搜索</button>{/if}
        </div>
      {:else if conversations.length === 0}
        <div class="no-results-state">
          <div class="no-results-icon">⌕</div>
          <strong>{search || activeConversationFilterCount ? '没有符合条件的对话' : '还没有可显示的对话'}</strong>
          <span>{search ? `没有找到“${search}”的结果。` : activeConversationFilterCount ? '当前筛选条件没有匹配项。' : '扫描完成后，历史对话会出现在这里。'}</span>
          {#if search || activeConversationFilterCount}<button class="secondary-button" on:click={() => void clearConversationFilters()}>清除筛选并返回全部</button>{/if}
        </div>
      {:else}
        <div class="asset-list conversation-list" role="list" aria-label="会话列表" bind:this={conversationListElement} bind:clientHeight={listViewportHeight} on:scroll={onConversationScroll}>
          <!-- 按实际 CSS 行高测量，虚拟列表随密度和浏览器字号同步。 -->
          <div class="conversation-row-size" aria-hidden="true" bind:clientHeight={measuredConversationRowHeight}></div>
          <div class="virtual-list-spacer" style={`height: ${displayedConversations.length * conversationRowHeight + (!searchIsApplied && conversationOffset < conversationTotal ? 40 : 0)}px`}>
            {#each visibleConversations as conversation, index (conversation.id)}
              <div class:selected={selectedConversation?.id === conversation.id} class:conversation-row-selected={selectedConversation?.id === conversation.id} class:conversation-child={conversation.familyDepth > 0} class="conversation-row-shell" style={`top: ${(visibleStart + index) * conversationRowHeight}px; height: ${conversationRowHeight}px; --family-depth: ${Math.min(3, conversation.familyDepth)}`} role="listitem">
                <label class="conversation-select" title={conversation.descendant_count ? `处理时一并纳入 ${conversation.descendant_count} 个子代理，执行前可核对` : conversation.thread_kind === 'subagent' ? '选择此子代理，保留父会话和兄弟线程' : '选择此对话；当前标签页刷新后保留选择'}>
                  <input type="checkbox" checked={selectedConversationIDs.has(conversation.id)} on:change={(event) => toggleConversationSelection(conversation.id, event.currentTarget.checked)} aria-label={`选择${conversation.title || '未命名对话'}`} />
                </label>
                {#if conversation.familyHasChildren}<button type="button" class="family-toggle" aria-label={`${conversation.familyExpanded ? '折叠' : '展开'}子代理：${conversation.title}`} aria-expanded={conversation.familyExpanded} on:click={() => toggleFamily(conversation.id)}><ChevronRight class={conversation.familyExpanded ? 'family-chevron-open' : ''} aria-hidden="true" /></button>{:else}<span class="family-toggle-spacer" aria-hidden="true"></span>{/if}
                <button class="conversation-row" aria-label={`打开对话：${conversation.title || '未命名对话'}`} aria-current={selectedConversation?.id === conversation.id ? 'true' : undefined} on:click={() => chooseConversation(conversation)}>
                  <div class="conversation-head"><span class="conversation-title" title={conversation.title || '未命名对话'}>{conversation.title || '未命名对话'}</span><time class="library-row-date" datetime={conversation.updated_at} title={formatTime(conversation.updated_at)}>{formatConversationDate(conversation.updated_at)}</time></div>
                  <div class="conversation-preview">{#if searchIsApplied && conversationHasSearchHit(conversation)}<span class="library-hit">命中</span>{/if}{conversation.first_user_text || conversation.agent_path || (conversation.thread_kind === 'subagent' ? '独立代理线程' : '暂无内容预览')}</div>
                  <div class="conversation-meta-row">{#if conversation.thread_kind === 'subagent' && !conversation.title?.startsWith('子代理')}<span class="family-label" title={conversation.agent_path || '子代理'}>子代理</span>{/if}{#if conversation.parent_missing}<span class="family-warning">父会话缺失</span>{/if}{#if conversation.descendant_count}<span class="family-label">子代理 {conversation.descendant_count}</span>{/if}<span class="conversation-project" title={conversation.cwd || '未关联项目'}><FolderOpen aria-hidden="true" />{conversation.cwd ? projectLabel(conversation.cwd) : '未关联项目'}</span><span class="library-row-tokens">{formatTokens(conversation.token_usage.total_tokens)} tokens</span><span class="library-row-state" title={statusLabels[conversation.status] ?? conversation.status}><span class={`mini-dot ${conversation.status}`}></span>{statusLabels[conversation.status] ?? conversation.status}</span>{#if conversation.diagnostics?.length}<span class="library-row-risk" title="存在诊断，需要复核"><TriangleAlert aria-hidden="true" /><span class="sr-only">需要复核</span></span>{/if}</div>
                </button>
              </div>
            {/each}
            {#if !searchIsApplied && conversationOffset < conversationTotal}
              <div class="virtual-load-more" style={`top: ${displayedConversations.length * conversationRowHeight}px`}>{conversationsLoadingMore ? '正在加载更多对话…' : `滚动加载更多（已载入 ${conversations.length} / ${conversationTotal}）`}</div>
            {/if}
          </div>
        </div>
      {/if}
    </section>

    <aside class="detail-column panel" class:detail-reading={detailView === 'reading'} class:detail-technical={detailView === 'technical'}>
      <div class="panel-heading library-reader-toolbar"><span>对话详情</span><button type="button" class="library-text-button" disabled={!selectedConversation} title="归档或删除前查看影响并确认" on:click={() => reviewPanelOpen = true}><ShieldCheck aria-hidden="true" />归档与删除</button></div>
      {#if selectedConversation}
        {#key `${report?.root || ''}:${selectedConversation.id}`}
        <div class="detail-content">
          {#snippet detailViewTabs()}
            <div class="detail-view-tabs" role="tablist" aria-label="详情视图"><button class:active={detailView === 'reading'} role="tab" aria-selected={detailView === 'reading'} on:click={showReadingDetails}>阅读</button><button class:active={detailView === 'technical'} role="tab" aria-selected={detailView === 'technical'} on:click={() => showTechnicalDetails()}>技术证据</button></div>
          {/snippet}
          <div class="detail-heading-row">
            <div class="detail-title-wrap"><span class="detail-kicker">会话</span><h2 class="detail-title" title={selectedConversation.title || '未命名对话'}>{selectedConversation.title || '未命名对话'}</h2></div>
            <button type="button" class="library-icon-button library-reader-back" aria-label="返回对话列表" on:click={() => selectWorkspaceView('sessions')}><ArrowLeft aria-hidden="true" /></button>
          </div>
          <div class="library-reader-meta"><span class={`status-pill ${selectedConversation.status}`}>{statusLabels[selectedConversation.status] ?? selectedConversation.status}</span><span class="library-reader-project" title={selectedConversation.cwd || '未关联项目'}><FolderOpen aria-hidden="true" />{selectedConversation.cwd ? projectLabel(selectedConversation.cwd) : '未关联项目'}</span><span>{formatTokens(selectedConversation.token_usage.total_tokens)} tokens</span><span>{selectedConversation.user_messages} 条提问</span></div>
          {#if detailView === 'technical'}{@render detailViewTabs()}{/if}
          {#if selectedConversation.thread_kind === 'subagent' || selectedConversation.thread_kind === 'automation' || selectedConversation.descendant_count || selectedConversation.family_conflict || selectedConversation.parent_conversation_id || selectedConversation.parent_missing}
            <div class="conversation-family" aria-label="会话关系">
              {#if selectedConversation.thread_kind === 'subagent'}<span class="family-label">子代理{selectedConversation.agent_nickname ? ` · ${selectedConversation.agent_nickname}` : ''}</span>{/if}
              {#if selectedConversation.thread_kind === 'automation'}<span class="family-label">{selectedConversation.automation_kind === 'memory_consolidation' ? '自动记忆任务' : '自动任务'}</span>{/if}
              {#if selectedConversation.family_conflict}<span class="family-warning">关系冲突，暂不可级联处理</span>{:else if selectedConversation.parent_conversation_id}<button type="button" class="library-text-button" on:click={() => openRelatedConversation(selectedConversation!.parent_conversation_id!)}>查看父会话<ChevronRight aria-hidden="true" /></button>{:else if selectedConversation.parent_missing}<span class="family-warning" title={selectedConversation.parent_thread_id}>父会话未找到</span>{/if}
              {#if selectedConversation.descendant_count}<details><summary>{selectedConversation.descendant_count} 个子代理（含后代）</summary><div class="family-children">{#each selectedConversation.child_conversation_ids || [] as childID}<button type="button" class="library-text-button" title={childID} on:click={() => openRelatedConversation(childID)}>{conversations.find(item => item.id === childID)?.title || '查看子代理'}<ChevronRight aria-hidden="true" /></button>{/each}</div><small>删除此会话时一并纳入；可单独选择子代理。</small></details>{/if}
            </div>
          {/if}
          {#if detailView === 'reading'}
          <ConversationReader conversationID={selectedConversation.id} workspace={report?.root || ''} {apiFetch} location={readerLocation}
            on:evidence={(event) => openReaderEvidence(event.detail)}>
          <svelte:fragment slot="view-tabs">{@render detailViewTabs()}</svelte:fragment>
          <button slot="summary-toggle" type="button" class="library-summary-toggle" class:active={summaryExpanded} aria-expanded={summaryExpanded} aria-controls="conversation-summary" title={conversationSummary ? '展开或收起已生成的摘要' : '展开摘要面板，按需生成'} on:click={() => summaryExpanded = !summaryExpanded}><ChevronRight aria-hidden="true" />对话摘要{#if conversationSummary}<span class="summary-ready" title="已生成摘要"><span class="sr-only">已生成</span></span>{/if}</button>
          <svelte:fragment slot="before-content">
          {#if summaryExpanded}
          <section id="conversation-summary" class="summary-panel library-summary-panel" aria-label="对话摘要">
            <p class="library-summary-help">{summaryProviderID === 'local-deterministic' ? '本地摘录元数据与前段事件，不调用模型，不等同于全文总结。' : '点击后调用已配置的摘要服务，仅发送脱敏后的有限事件窗口。'}结果仅保留在当前页面，刷新后清除。</p>
            <div class="summary-panel-head"><div><span class="subheading">摘要</span>{#if conversationSummary}<span class="summary-meta">{summaryProviderLabel(conversationSummary.provider_id)} · 置信度 {summaryConfidenceLabel(conversationSummary.confidence)}</span>{/if}</div><div class="summary-panel-actions">{#if summaryProviderIDs.length > 1}<select bind:value={summaryProviderID} aria-label="摘要生成方式">{#each summaryProviderIDs as providerID}<option value={providerID}>{summaryProviderLabel(providerID)}</option>{/each}</select>{/if}<button class="summary-button" on:click={loadConversationSummary} disabled={summaryLoading}>{summaryLoading ? '生成中…' : conversationSummary ? '重新生成' : summaryProviderID === 'openai-compatible' ? '生成云端摘要' : '生成摘要'}</button></div></div>
            {#if conversationSummary}
              <div class="summary-text">{conversationSummary.text}</div>
              <div class="summary-meta summary-meta-wrap"><span>依据：{conversationSummary.evidence?.map((item) => item.label).join('、') || '无'}</span>{#if conversationSummary.redacted}<span>已脱敏 {conversationSummary.redaction_count ?? 0} 处</span>{/if}</div>
              {#if conversationSummary.limitations?.length}<div class="summary-limitations">{#each conversationSummary.limitations as limitation}<span>{limitation}</span>{/each}</div>{/if}
            {:else}
              <div class="summary-empty">尚未生成摘要</div>
            {/if}
            {#if summaryMessage}<p class="summary-message" role="status">{summaryMessage}</p>{/if}
          </section>
          {/if}
          </svelte:fragment>
          </ConversationReader>
          {#if searchHits.length}
            <div class="subheading">正文命中（{searchHits.length}）</div>
            {#each searchHits.filter((hit) => selectedConversation && (hit.conversation_id === selectedConversation.id || selectedConversation.rollout_paths.includes(hit.relative_path))).slice(0, 12) as hit}
              <button class="search-hit" on:click={() => openSearchHit(hit)} title="定位到命中的消息">
                <div class="search-hit-head"><strong>{hit.relative_path} · 第 {hit.line} 行</strong>{#if hit.activity_status}<span class={`status-pill ${hit.activity_status}`}>{statusLabels[hit.activity_status] ?? hit.activity_status}</span>{/if}</div>
                <span>{hit.snippet}</span>
                <small>字节 {hit.start}–{hit.end} · 点击定位消息 · {hit.live_consistency ? `读取一致性：${liveConsistencyLabels[hit.live_consistency] ?? hit.live_consistency}` : hit.read_only ? '只读结果' : '状态未知'}</small>
              </button>
            {/each}
          {/if}

          {:else}
          <details class="library-context"><summary>会话信息与项目关系</summary>
          <details class="identity-details">
            <summary>完整标题、标识与路径</summary>
            <dl>
              <div><dt>完整标题</dt><dd><span class="identity-value">{selectedConversation.title || '未命名对话'}</span>{#if selectedConversation.title}<button class="copy-inline" title="复制完整标题" on:click={() => void copyText(selectedConversation?.title, '完整标题')}>{copiedValue === '完整标题' ? '已复制' : '复制'}</button>{/if}</dd></div>
              <div><dt>会话 ID</dt><dd><code>{selectedConversation.id}</code><button class="copy-inline" title="复制完整会话 ID" on:click={() => void copyText(selectedConversation?.id, '会话 ID')}>{copiedValue === '会话 ID' ? '已复制' : '复制'}</button></dd></div>
              {#if selectedConversation.cli_version}<div><dt>CLI 版本</dt><dd><code>{selectedConversation.cli_version}</code></dd></div>{/if}
              <div><dt>工作目录</dt><dd><code>{selectedConversation.cwd || '未关联项目'}</code>{#if selectedConversation.cwd}<button class="copy-inline" title="复制完整工作目录" on:click={() => void copyText(selectedConversation?.cwd, '工作目录')}>{copiedValue === '工作目录' ? '已复制' : '复制'}</button>{/if}</dd></div>
              <div><dt>关联 rollout</dt><dd>{#if selectedConversation.rollout_paths.length}<div class="identity-path-list">{#each selectedConversation.rollout_paths as path}<div class="identity-path-row"><code title={path}>{path}</code><button class="copy-inline" title="复制 rollout 路径" on:click={() => void copyText(path, `rollout 路径：${path}`)}>{copiedValue === `rollout 路径：${path}` ? '已复制' : '复制'}</button></div>{/each}</div>{:else}无{/if}</dd></div>
            </dl>
          </details>
          <details class="project-disclosure">
            <summary>项目关系</summary>
            <div class="project-panel">
            <div class="project-panel-head">
              <div><span class="subheading">项目关系</span>{#if projectRelation}<span class="summary-meta">{projectStatusLabel(projectRelation.status)} · 置信度 {projectConfidenceLabel(projectRelation.confidence)}</span>{/if}</div>
              <button class="summary-button" on:click={loadProjectRelation} disabled={projectLoading}>{projectLoading ? '扫描中…' : projectRelation ? '重新扫描' : '扫描项目关系'}</button>
            </div>
            {#if projectRelation}
              <div class="project-meta"><span class="project-root-value" title={projectRelation.project_root || undefined}>{formatPath(projectRelation.project_root, 3)}</span>{#if projectRelation.project_root}<button class="copy-inline" title="复制项目路径" on:click={() => void copyText(projectRelation?.project_root, '项目路径')}>{copiedValue === '项目路径' ? '已复制' : '复制路径'}</button>{/if}<span>{projectRelation.file_count} 个文件 · {formatBytes(projectRelation.total_bytes)}</span><span>忽略 {projectRelation.ignored_file_count}</span>{#if projectRelation.symlink_count}<span>符号链接 {projectRelation.symlink_count}</span>{/if}{#if projectRelation.truncated}<span class="project-warning">已达到扫描预算</span>{/if}</div>
              <dl class="project-facts">
                <div><dt>Git 根目录</dt><dd title={projectRelation.git_root}>{projectRelation.git_root || '未发现'}</dd></div>
                <div><dt>分支</dt><dd>{projectRelation.git_branch || '未知'}</dd></div>
                <div><dt>远程仓库</dt><dd title={projectRelation.git_remote}>{projectRelation.git_remote || '未发现'}</dd></div>
                <div><dt>工作目录相对路径</dt><dd>{projectRelation.working_directory_relative || '.'}</dd></div>
              </dl>
              {#if projectRelation.key_files?.length}
                <details class="project-details" open>
                  <summary>关键文件（{projectRelation.key_files.length}）</summary>
                  <div class="project-file-list">{#each projectRelation.key_files as file}<div><span>{file.relative_path}</span><small>{file.kind} · {formatBytes(file.size_bytes)}</small></div>{/each}</div>
                </details>
              {/if}
              {#if projectRelation.recent_files?.length}
                <details class="project-details">
                  <summary>最近修改文件（{projectRelation.recent_files.length}）</summary>
                  <div class="project-file-list">{#each projectRelation.recent_files as file}<div><span>{file.relative_path}</span><small>{formatTime(file.mod_time)} · {formatBytes(file.size_bytes)}</small></div>{/each}</div>
                </details>
              {/if}
              {#if projectRelation.evidence?.length}
                <details class="project-details"><summary>关系证据（{projectRelation.evidence.length}）</summary><div class="project-evidence-list">{#each projectRelation.evidence as evidence}<span><strong>{evidence.kind}</strong>{evidence.value} · 置信度 {projectConfidenceLabel(evidence.confidence)}</span>{/each}</div></details>
              {/if}
              {#if projectRelation.limitations?.length}<div class="project-limitations">{#each projectRelation.limitations as limitation}<span>{limitation}</span>{/each}</div>{/if}
              <small class="evidence-note">仅读取项目文件元数据和 Git 信息，不读取项目正文，不修改项目目录；结果不会写入 `.codex` 或 archive.db。</small>
            {:else}
              <div class="summary-empty">按需读取项目与 Git 信息</div>
            {/if}
            {#if projectMessage}<div class="summary-message">{projectMessage}</div>{/if}
            </div>
          </details>
          </details>
          <dl class="facts">
            <div><dt>更新时间</dt><dd>{formatTime(selectedConversation.updated_at)}</dd></div>
            <div><dt>工作目录</dt><dd title={selectedConversation.cwd}>{selectedConversation.cwd || '未知'}</dd></div>
            <div><dt>消息 / 回合</dt><dd>{selectedConversation.user_messages} / {selectedConversation.turn_count}</dd></div>
            <div><dt>累计 tokens</dt><dd>{formatTokens(selectedConversation.token_usage.total_tokens)}</dd></div>
            <div><dt>模型</dt><dd>{selectedConversation.model || '未知'}</dd></div>
            <div><dt>资产数量</dt><dd>{selectedAssets.length}</dd></div>
          </dl>

          {#if selectedConversation.asset_graph}
            <div class="asset-graph-panel">
              <div class="subheading">Canonical 资产图</div>
              <div class="asset-graph-summary">
                <span class={`graph-completeness ${selectedConversation.asset_graph.completeness}`}>
                  {graphCompletenessLabels[selectedConversation.asset_graph.completeness] || selectedConversation.asset_graph.completeness}
                </span>
                <span>{selectedConversation.asset_graph.nodes.length} / {assetGraphNodeTotal || selectedConversation.asset_graph.nodes.length} 个节点</span>
                <span>{selectedConversation.asset_graph.edges.length} / {assetGraphEdgeTotal || selectedConversation.asset_graph.edges.length} 条关系</span>
                <span title={selectedConversation.asset_graph.hash}>哈希 {selectedConversation.asset_graph.hash.slice(0, 12)}…</span>
              </div>
              <div class="asset-graph-kinds">
                {#each graphNodeCounts(selectedConversation.asset_graph) as item}
                  <span><strong>{item.count}</strong>{item.label}</span>
                {/each}
              </div>
              {#if selectedConversation.asset_graph.diagnostics?.length}
                <div class="asset-graph-diagnostics">
                  {#each selectedConversation.asset_graph.diagnostics as diagnostic}
                    <span><strong>{diagnostic.code}</strong>{diagnostic.message}</span>
                  {/each}
                </div>
              {/if}
              <details class="asset-graph-details">
                <summary>展开节点、关系和证据</summary>
                <div class="asset-graph-list">
                  {#each selectedConversation.asset_graph.nodes as node (node.id)}
                    <div class="asset-graph-node">
                      <div><span class="graph-kind">{graphKindLabels[node.kind] || node.kind}</span><strong>{graphNodeLabel(selectedConversation.asset_graph, node.id)}</strong><span class={`status-pill ${node.status}`}>{statusLabels[node.status] ?? node.status}</span></div>
                      {#if graphNodeDetail(node)}<small>{graphNodeDetail(node)}</small>{/if}
                    </div>
                  {/each}
                </div>
                <div class="asset-graph-edges">
                  {#each selectedConversation.asset_graph.edges as edge}
                    <div><strong>{graphNodeLabel(selectedConversation.asset_graph, edge.from)}</strong><span>{edge.relation}</span><strong>{graphNodeLabel(selectedConversation.asset_graph, edge.to)}</strong><small>{edge.evidence} · 置信度 {edge.confidence}</small></div>
                  {/each}
                </div>
              </details>
              {#if assetGraphHasMore}
                <button class="secondary-button load-more-button" on:click={() => loadAssetGraph(false)} disabled={assetGraphLoadingMore}>{assetGraphLoadingMore ? '正在加载更多资产图…' : '加载更多资产图'}</button>
              {/if}
              {#if assetGraphMessage}<div class="maintenance-message">{assetGraphMessage}</div>{/if}
            </div>
          {:else if assetGraphLoading}
            <div class="summary-empty">正在按页读取资产图…</div>
          {:else if assetGraphMessage}
            <div class="summary-message">{assetGraphMessage}</div>
          {/if}

          <div class="activity-summary-panel">
            <div class="activity-summary-head"><div><span class="subheading">运行时读取状态</span>{#if conversationActivity}<span class="summary-meta">最近探测 {formatTime(conversationActivity.activity.generated_at)}</span>{/if}</div><button class="summary-button" on:click={loadConversationActivity} disabled={activityLoading}>{activityLoading ? '探测中…' : conversationActivity ? '重新探测' : '读取活动状态'}</button></div>
            {#if conversationActivity}
              <div class="activity-summary-status"><span class:blocked={conversationActivity.lifecycle_blocked} class="activity-dot"></span><strong>{conversationActivity.lifecycle_blocked ? '生命周期操作已阻断' : '当前可继续只读阅读'}</strong><span>{conversationActivity.readable ? '源文件可读取' : '部分源文件不可读取'}</span><span>{conversationActivity.activity.window_count} 个窗口</span></div>
              {#if conversationActivity.activity.windows?.length}<div class="activity-window-strip">{#each conversationActivity.activity.windows as window}<span title={`窗口 ${window.index} · ${formatTime(window.probed_at)}`}>窗口 {window.index}：{window.changed_count ? `${window.changed_count} 项变化` : '无变化'} · {window.blocked_count} 项阻断</span>{/each}</div>{/if}
              <div class="activity-result-list">
                {#each conversationActivity.activity.results as item}
                  <div class={`activity-result-row ${item.status}`}>
                    <div><span class={`status-pill ${item.activity_status || item.status}`}>{statusLabels[item.activity_status || item.status] ?? (item.activity_status || item.status)}</span><strong title={item.relative_path}>{item.relative_path}</strong></div>
                    <small>{item.evidence.reason || '探测窗口内未发现变化'} · {item.evidence.readable ? '可读' : '不可读'} · {item.mutation_blocked ? '禁止生命周期操作' : '未发现写入阻断'}</small>
                    {#if item.evidence.observed_states?.length}<small>证据：{item.evidence.observed_states.map((state) => statusLabels[state] ?? state).join('、')}</small>{/if}
                    {#if item.evidence.open_by_process?.length}<small>打开进程：{item.evidence.open_by_process.map((process) => `${process.command || '进程'} (${process.pid})`).join('、')}</small>{/if}
                    {#if item.evidence.matching_lock_ids?.length}<small>匹配锁：{item.evidence.matching_lock_ids.join('、')}</small>{/if}
                    {#if item.evidence.sqlite_wal_present || item.evidence.sqlite_shm_present}<small>SQLite 旁路文件：{item.evidence.sqlite_auxiliary?.join('、') || 'WAL/SHM'}</small>{/if}
                    {#if item.evidence.source_hash_compared}<small>已比较源哈希：{item.evidence.source_hash_algorithm || 'SHA-256'}</small>{/if}
                  </div>
                {/each}
              </div>
            {:else if activityLoading}
              <div class="summary-empty">正在读取文件活动、尾行、进程、锁和 SQLite 旁路证据…</div>
            {:else}
              <div class="summary-empty">只读探测不会修改 `.codex`；点击后可识别追加、半截 JSONL、打开进程、writer lock 和 WAL。</div>
            {/if}
            {#if activityMessage}<div class="summary-message">{activityMessage}</div>{/if}
          </div>
          {#if selectedConversation.diagnostics?.length}
            <div class="subheading">诊断证据</div>
            {#each selectedConversation.diagnostics as diagnostic}
              <div class={`diagnostic ${diagnostic.severity}`}><strong>{diagnostic.code}</strong><span>{diagnostic.message}</span>{#if diagnostic.line}<small>第 {diagnostic.line} 行 · {diagnostic.evidence || ''}</small>{/if}</div>
            {/each}
          {/if}

          <div class="subheading">关联文件与资产</div>
          <div class="detail-assets">
            {#each selectedAssets as file (file.relative_path)}
              <div class="detail-asset-row-shell">
                <button class:selected={selectedAsset?.relative_path === file.relative_path} class="detail-asset-row" title={file.relative_path} aria-label={`查看资产 ${file.relative_path}`} on:click={() => chooseAsset(file)}>
                  <span class="asset-type">{file.kind === 'sqlite' ? 'SQLITE' : file.kind === 'rollout_jsonl' ? 'JSONL' : file.kind === 'session_index_jsonl' ? 'INDEX' : 'FILE'}</span><span class="asset-path">{file.relative_path}</span><span class={`status-pill ${file.status}`}>{statusLabels[file.status] ?? file.status}</span>
                </button>
                <button class="copy-inline asset-copy-button" title="复制资产路径" on:click|stopPropagation={() => void copyText(file.relative_path, `资产路径：${file.relative_path}`)}>{copiedValue === `资产路径：${file.relative_path}` ? '已复制' : '复制路径'}</button>
              </div>
            {/each}
          </div>

          <details class="library-transcript library-system-events">
            <summary><span>工具与系统事件</span><small>{eventsLoading ? '正在读取…' : `已载入 ${technicalEvents.length} 项`}</small></summary>
            {#if eventsLoading}<div class="empty-note">正在读取事件…</div>{/if}
            <div class="event-timeline">
              {#each technicalEvents as event (event.id)}
                <details class="event-card system">
                  <summary class="event-card-head"><strong>{event.kind === 'injected_context' ? '注入上下文' : event.kind}</strong><span>第 {event.line} 行 · 字节 {event.start_offset}–{event.end_offset}</span></summary>
                  <div class="event-text">{event.text}</div>
                  {#if event.truncated}<small>仅显示分段预览。</small>{/if}
                </details>
              {:else}{#if !eventsLoading}<div class="empty-note">当前事件页没有工具或系统事件。</div>{/if}{/each}
            </div>
            {#if eventTotal > events.length}<button class="secondary-button load-more-button" on:click={loadMoreEvents} disabled={eventsLoadingMore}>{eventsLoadingMore ? '加载中…' : '加载后续事件'}</button>{/if}
          </details>
          {#if selectedAsset}
            <div class="subheading">原始片段 · {selectedAsset.relative_path}{#if previewLength} · 字节 {previewStart}–{previewStart + previewLength}{/if}</div>
            <pre class="source-preview">{preview || '正在读取片段…'}</pre>
            <div class="evidence-note">片段通过固定根目录和字节范围读取，未将整文件载入页面。</div>
          {/if}
          {/if}
        </div>
        {/key}
      {:else if routeConversationLoading}
        <div class="reader-empty" role="status"><strong>正在打开对话</strong><span>{compactID(routeConversationID)}</span></div>
      {:else if routeConversationError}
        <div class="reader-empty" role="alert"><strong>无法打开这个对话</strong><span>{routeConversationError}</span><button type="button" class="secondary-button" on:click={() => void restoreConversationFromRoute(routeConversationID)}>重试</button></div>
      {:else}
        <div class="reader-empty"><svg viewBox="0 0 48 48" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><path d="M8 10h13a5 5 0 0 1 5 5v25a8 8 0 0 0-8-4H8V10Zm32 0H29a5 5 0 0 0-5 5v25a8 8 0 0 1 8-4h8V10Z" /></svg><strong>打开一段历史对话</strong><span>选择左侧会话开始阅读</span></div>
      {/if}
    </aside>
  </main>

  <LibraryDrawer bind:open={conversationFiltersOpen} title="筛选会话" description="应用后更新列表，条件会保存在网址中。">
    <form class="library-filter-form" on:submit|preventDefault={applyConversationFilters}>
      <label>正文搜索<input type="search" bind:value={filterDraft.search} placeholder="搜索对话正文" /></label>
      <label>标题与元数据<input bind:value={filterDraft.query} placeholder="标题、首条消息、路径或 ID" /></label>
      <label>工作目录<input bind:value={filterDraft.cwd} placeholder="完整路径或目录关键词" /></label>
      <label>模型<input bind:value={filterDraft.model} placeholder="模型或提供方" /></label>
      <label>对话状态<select bind:value={filterDraft.status}><option value="全部状态">全部状态</option>{#each Object.entries(statusLabels) as [status, label]}<option value={status}>{label}</option>{/each}</select></label>
      <label>诊断状态<select bind:value={filterDraft.diagnostics}><option value="">全部</option><option value="true">有诊断</option><option value="false">无诊断</option></select></label>
      <fieldset><legend>更新时间</legend><label>起点<input type="date" bind:value={filterDraft.from} /></label><label>终点<input type="date" bind:value={filterDraft.to} /></label></fieldset>
      <fieldset><legend>token 数</legend><label>最少<input inputmode="numeric" pattern="[0-9]*" bind:value={filterDraft.minTokens} placeholder="不限" /></label><label>最多<input inputmode="numeric" pattern="[0-9]*" bind:value={filterDraft.maxTokens} placeholder="不限" /></label></fieldset>
      <fieldset><legend>资产大小（字节）</legend><label>最小<input inputmode="numeric" pattern="[0-9]*" bind:value={filterDraft.minBytes} placeholder="不限" /></label><label>最大<input inputmode="numeric" pattern="[0-9]*" bind:value={filterDraft.maxBytes} placeholder="不限" /></label></fieldset>
      <div class="library-form-actions"><button type="button" class="secondary-button" on:click={() => filterDraft = emptySessionFilters()}>重置条件</button><button class="filter-apply-button" type="submit" disabled={conversationsLoading}>{conversationsLoading ? '筛选中…' : '应用筛选'}</button></div>
    </form>
  </LibraryDrawer>
  <LibraryDrawer bind:open={batchPanelOpen} title="批量处理" description={selectedConversationCount ? `已选择 ${selectedConversationCount} 项 · 刷新后保留未完成的选择` : executionResult ? '本次执行结果' : '当前没有待处理的选择'}>
    {#if executionTask?.state === 'running'}<ExecutionProgress task={executionTask} message={executionMessage} on:stop={() => executionTask && stopExecutionTask(executionTask)} on:refresh={loadExecutionTasks} />{/if}
    {#if executionResult}<div class="operation-outcome" role="status"><span>{executionResultLabel(executionResult)}</span><button type="button" class="library-text-button" on:click={() => selectWorkspaceView('pending')}>查看执行记录</button></div>{/if}
    {#if selectedConversationCount}
    <OperationChoices options={operationOptions} trashOnly={lifecycleRestricted} realConversations={lifecycleRealConversations} bind:archivePassword={operationArchivePassword} summary={operationSummary(operationOptions)} writesEnabled={lifecycleWritesEnabled} on:optionchange={(event) => setOperationOption(event.detail.option, event.detail.checked)} on:invalidate={invalidateOperationPreview} />
    {/if}
        <div class="batch-toolbar">
        {#if selectedConversationCount || batchOperationPreview}
        <div class="batch-toolbar-head">
          <div><strong>批量审核</strong><span class="muted">已载入 {conversations.length} 项</span></div>
          <span class="batch-selection-count">已选 {selectedConversationCount} 项</span>
        </div>
        <div class="batch-toolbar-actions">
          <button class="secondary-button" on:click={selectLoadedConversations} disabled={!conversations.length || batchOperationLoading || allLoadedConversationsSelected}>全选已载入</button>
          {#if selectedConversationCount}<button class="secondary-button" on:click={clearConversationSelection} disabled={batchOperationLoading}>清空选择</button>{/if}
          <button class="secondary-button" on:click={copySelectedConversationIDs} disabled={selectionCopyLoading || !selectedConversationCount}>{selectionCopyLoading ? '核对 ID…' : '复制所选原始 ID'}</button>
          <button class="summary-batch-button" on:click={requestBatchSummary} disabled={summaryBatchLoading || selectedConversationCount === 0}>{summaryBatchLoading ? '摘要生成中…' : '生成摘要'}</button>
        </div>
        <div class="batch-selection-meta">已载入的所选会话 tokens：{formatTokens(selectedConversationTokenTotal)} · 选择可跨筛选保留，执行前必须重新预览</div>
        {#if selectionStorageError}<p class="evidence-note" role="status">浏览器未能保存选择；当前选择仍可使用，但刷新可能丢失。</p>{/if}
        {#if selectionCopyMessage}<p class="evidence-note" role="status">{selectionCopyMessage}</p>{/if}
        {#if selectedConversationCount}<details class="batch-evidence batch-selected-list"><summary>核对所选会话标识（{selectedConversationCount}）</summary>{#each Array.from(selectedConversationIDs) as id}{@const item = conversations.find((conversation) => conversation.id === id) || latestBatchReview?.preview.items.find((conversation) => conversation.conversation_id === id)}<div><span>{item?.title || '尚未载入会话'}</span><code>{item?.source_id || id}</code></div>{/each}</details>{/if}
        {#if batchOperationMessage}<div class:batch-message-blocked={Boolean(batchOperationPreview?.blocked || executionMessage && batchOperationMessage === executionMessage)} class="batch-operation-message" role="status">{batchOperationMessage}</div>{/if}
        {#if batchOperationPreview}
          <div bind:this={batchImpactPanel} class:batch-preview-blocked={batchOperationPreview.blocked} class="batch-preview-panel">
            <div class="batch-preview-head">
              <strong>{batchOperationPreview.items.some((item) => item.completed) ? '批量执行结果' : '批量影响预览'}</strong>
              <span class={`status-pill ${batchOperationPreview.blocked ? 'conflict' : 'stable'}`}>{batchOperationPreview.blocked ? '检查未通过' : '检查通过'}</span>
            </div>
            {#if batchOperationPreview.archive_preview}<ArchiveImpact preview={batchOperationPreview.archive_preview} local={Boolean(effectiveOperationOptions().archive_local)} remote={Boolean(operationOptions.archive_remote)} encrypted={operationOptions.encrypted} {formatBytes} />{/if}
            <div class="batch-summary">
              <span>{batchOperationPreview.summary.conversations} 个对话</span>
              {#if batchOperationPreview.items.some(item => item.automatically_included)}<span>含 {batchOperationPreview.items.filter(item => item.automatically_included).length} 个自动纳入的子代理</span>{/if}
              <span>{batchOperationPreview.summary.prepared} 个可执行</span>
              <span>{batchOperationPreview.summary.blocked} 个未通过</span>
              {#if batchOperationPreview.restricted_preview}<span>计划处理文件 {formatBytes(batchOperationPreview.summary.bytes)}</span>{/if}
            </div>
            {#if batchSharedError}<p class="batch-item-error" role="status">{impactBlockerMessage(batchSharedError)}</p>{/if}
            <div class="batch-items">
              {#each reviewFamilyItems(batchOperationPreview.items) as item (item.conversation_id)}
                <div class:batch-item-blocked={item.blocked} class:batch-item-complete={item.completed} class="batch-item">
                  <div class="batch-item-head">
                    <span class={`status-pill ${item.completed ? 'stable' : item.blocked ? 'conflict' : 'plan-prepared'}`}>{item.completed ? '已完成' : item.blocked ? '未通过' : operationPlanStatusLabels[item.plan?.status || 'prepared'] || '可执行'}</span>
                    <strong title={item.conversation_id}>{item.automatically_included ? '自动纳入 · ' : ''}{item.thread_kind === 'subagent' && !item.title?.startsWith('子代理') ? '子代理 · ' : ''}{item.title || item.conversation_id}</strong>
                  </div>
                  {#if item.error && !batchSharedError}<p class="batch-item-error">{impactBlockerMessage(item.error)}</p>{/if}
                  <OperationImpact sourceID={item.source_id || item.conversation_id} files={item.plan?.assets || []} discoveredFiles={item.discovered_assets || []} preview={batchOperationPreview.restricted_preview} action={operationSummary(operationOptions)} prepared={item.plan?.status === 'prepared' && !item.blocked} completed={item.completed || false} {formatBytes} />
                  <details class="batch-evidence operation-technical">
                    <summary>技术详情</summary>
                    <div><span>会话 ID</span><code>{item.source_id || item.conversation_id}</code></div>
                    {#if item.asset_graph}<div><span>关联检查：{graphCompletenessLabels[item.asset_graph.completeness] || item.asset_graph.completeness}</span><small>{item.asset_graph.nodes.length} 个数据节点 · {item.asset_graph.edges.length} 条关联，包含数据库记录，不是文件数量。</small></div>{/if}
                    {#if item.discovered_assets?.length}<details><summary>扫描到的关联文件</summary>{#each item.discovered_assets as asset}<div class="technical-file"><code>{asset.relative_path}</code><small>整文件 {formatBytes(asset.size_bytes)}{asset.kind === 'sqlite' ? ' · 数据库保留，具体行影响见文件与记录清单' : asset.owners > 1 ? ' · 多个会话共用' : ''}</small></div>{/each}</details>{/if}
                    {#if item.activity}<details><summary>活动检查结果</summary>{#each item.activity.results as result}<div class="technical-file"><code>{result.relative_path}</code><small>{statusLabels[result.status] || result.status}{result.evidence.reason ? ` · ${result.evidence.reason}` : ''}</small></div>{/each}</details>{/if}
                    {#if item.error}<div><span>完整阻断原因</span><code>{item.error}</code></div>{/if}
                    {#if item.result?.journal_path}<div><span>执行日志</span><code>{item.result.journal_path}</code></div>{/if}
                  </details>
                </div>
              {/each}
            </div>
            {#if batchOperationPreview.restricted_preview}
              <p class="operation-space-note">{batchOperationPreview.restricted_preview.trash_package ? '目标数据将汇入一个回收文件夹，成功后清理临时备份。清空回收站前仍占磁盘空间；共享数据库不会自动缩小。' : operationOptions.trash ? '回收站内容仍占磁盘空间；共享数据库不会自动缩小。' : '清理记录不会自动缩小共享数据库。'}</p>
              <details class="batch-evidence operation-technical"><summary>恢复位置与完整技术数据</summary><div><span>{batchOperationPreview.restricted_preview.trash_package ? '临时材料位置（成功后只留执行凭据）' : batchOperationPreview.restricted_preview.permanent_delete ? '成功后清理临时恢复正文；失败时保留供复核' : '恢复材料保存在源目录之外'}</span><code>{batchOperationPreview.restricted_preview.recovery_path}</code></div><details class="restricted-impact" bind:open={batchRawPreviewOpen}><summary>原始预览 JSON</summary>{#if batchRawPreviewOpen}<pre>{JSON.stringify(batchOperationPreview.restricted_preview, null, 2)}</pre>{/if}</details></details>
            {/if}
          </div>
        {/if}
        {/if}
        {#if summaryMessage}<div class="summary-message">{summaryMessage}</div>{/if}
        {#if summaryTask}
          <div class="summary-task-panel">
            <div class="summary-task-head"><strong>批量摘要任务</strong><span class={`status-pill ${summaryTask.status === 'completed' ? 'stable' : summaryTask.status === 'failed' ? 'conflict' : 'active_writing'}`}>{summaryTaskStatusLabel(summaryTask.status)}</span></div>
                  <div class="summary-task-progress"><span>{summaryTask.completed} / {summaryTask.requested} 已完成</span><span>{summaryTask.failed} 项失败</span>{#if summaryTask.current_conversation_id}<span title={summaryTask.current_conversation_id}>当前：{compactID(summaryTask.current_conversation_id)}</span>{/if}</div>
                  <div class="summary-task-progress-bar" role="progressbar" aria-label="批量摘要进度" aria-valuemin="0" aria-valuemax={summaryTask.requested} aria-valuenow={summaryTask.completed}><i style={`width: ${Math.min(100, summaryTask.requested ? summaryTask.completed / summaryTask.requested * 100 : 0)}%`}></i></div>
            {#if summaryTask.errors?.length}<div class="summary-task-errors">{#each summaryTask.errors as item}<span>{item.conversation_id}：{item.error}</span>{/each}</div>{/if}
            <small class="evidence-note">摘要来自 {summaryProviderLabel(summaryTask.provider_id)} provider，结果不会写入 `.codex` 或 archive.db。</small>
          </div>
        {/if}
        </div>
    <div slot="footer">{#if selectedConversationCount}<OperationWorkflow destructive={operationOptions.trash || operationOptions.permanent_delete} count={selectedConversationCount} selected={operationChosen} selectionError={operationSelectionError} previewed={Boolean(batchOperationPreview)} blocked={Boolean(batchOperationPreview?.blocked)} ready={batchOperationExecutable} loading={batchOperationLoading && !batchOperationExecuting} confirming={executionBusy || destinationBusy} running={batchOperationExecuting} writesEnabled={lifecycleWritesEnabled} needsPassword={operationOptions.encrypted && !operationArchivePassword.trim()} message={batchOperationPreview?.blocked ? '请查看未通过会话的原因，处理后重新检查。' : batchOperationMessage} on:preview={loadBatchOperationPreview} on:confirm={executeBatchOperation} />{:else}<button type="button" class="secondary-button" on:click={() => batchPanelOpen = false}>返回会话库</button>{/if}</div>
  </LibraryDrawer>
  <LibraryDrawer bind:open={reviewPanelOpen} title="归档与删除">
    {#if selectedConversation}
      <div class="library-review-identity"><strong title={selectedConversation.title || '未命名对话'}>{selectedConversation.title || '未命名对话'}</strong><code>{selectedConversation.source_id || selectedConversation.id}</code><span class={`status-pill ${selectedConversation.status}`}>{statusLabels[selectedConversation.status] ?? selectedConversation.status}</span></div>
      {#if selectedConversation.parent_thread_id}<p class="operation-space-note">仅处理当前子代理及其后代，保留父会话和兄弟线程。</p>{/if}
      {#if executionTask?.state === 'running'}<ExecutionProgress task={executionTask} message={executionMessage} on:stop={() => executionTask && stopExecutionTask(executionTask)} on:refresh={loadExecutionTasks} />{/if}
      <OperationChoices options={operationOptions} trashOnly={lifecycleRestricted} realConversations={lifecycleRealConversations} bind:archivePassword={operationArchivePassword} summary={operationSummary(operationOptions)} writesEnabled={lifecycleWritesEnabled} on:optionchange={(event) => setOperationOption(event.detail.option, event.detail.checked)} on:invalidate={invalidateOperationPreview} />
      <div class="library-review-actions"><button type="button" class="secondary-button" on:click={stageCurrentOperation} disabled={pendingBusy !== ''}>{pendingBusy ? '保存中…' : '暂存操作'}</button></div>
          {#if impactPreview}
            <div bind:this={singleImpactPanel} class="impact-panel">
              <div class="batch-preview-head"><strong>操作影响</strong><span class={`status-pill ${impactPreview.blocked ? 'conflict' : 'stable'}`}>{impactPreview.blocked ? '未通过检查' : operationPlanStatusLabels[impactPreview.plan?.status || 'prepared'] || '已准备'}</span></div>
              {#if impactPreview.archive_preview}<ArchiveImpact preview={impactPreview.archive_preview} local={Boolean(effectiveOperationOptions().archive_local)} remote={Boolean(operationOptions.archive_remote)} encrypted={operationOptions.encrypted} {formatBytes} />{/if}
              {#if impactPreview.blocked}<p class="batch-item-error">{impactBlockerMessage(impactPreview.error || '当前计划无法执行，请查看技术详情。')}</p>{/if}
              {#if impactPreview.items && impactPreview.items.length > 1}
                <p class="operation-space-note">本次共 {impactPreview.items.length} 个线程，包含 {impactPreview.items.filter(item => item.automatically_included).length} 个自动纳入的子代理。以下全部对象将一并处理。</p>
                {#each reviewFamilyItems(impactPreview.items) as item}<details class="family-impact" open><summary>{item.automatically_included ? '自动纳入 · ' : ''}{item.thread_kind === 'subagent' && !item.title?.startsWith('子代理') ? '子代理 · ' : ''}{item.title || '未命名对话'}</summary><OperationImpact sourceID={item.source_id || item.conversation_id} files={item.plan?.assets || []} preview={impactPreview.restricted_preview} action={operationSummary(operationOptions)} prepared={item.plan?.status === 'prepared' && !item.blocked} {formatBytes} /></details>{/each}
              {:else}<OperationImpact sourceID={selectedConversation.source_id || selectedConversation.id} files={impactPreview.plan?.assets || []} preview={impactPreview.restricted_preview} action={operationSummary(operationOptions)} prepared={impactPreview.plan?.status === 'prepared' && !impactPreview.blocked} {formatBytes} />{/if}
              {#if impactPreview.restricted_preview}<p class="operation-space-note">{impactPreview.restricted_preview.trash_package ? '目标数据将汇入一个回收文件夹，成功后清理临时备份。清空回收站前仍占磁盘空间；共享数据库不会自动缩小。' : operationOptions.trash ? '回收站内容仍占磁盘空间；共享数据库不会自动缩小。' : '清理记录不会自动缩小共享数据库。'}</p>{/if}
              <details class="batch-evidence operation-technical" bind:open={singleTechnicalOpen}>
                <summary>技术详情与恢复位置</summary>
                {#if singleTechnicalOpen}
                  {#if impactPreview.error}<div><span>完整阻断原因</span><code>{impactPreview.error}</code></div>{/if}
                  {#if impactPreview.asset_graph}
                    <div><span>关联检查：{graphCompletenessLabels[impactPreview.asset_graph.completeness] || impactPreview.asset_graph.completeness}</span><small>{impactPreview.asset_graph.nodes.length} 个数据节点 · {impactPreview.asset_graph.edges.length} 条关联，包含数据库记录，不是文件数量。</small></div>
                    <details class="restricted-impact"><summary>完整关联图（JSON）</summary><pre>{JSON.stringify(impactPreview.asset_graph, null, 2)}</pre></details>
                  {/if}
                  {#if impactPreview.activity}<details><summary>活动检查结果</summary>{#each impactPreview.activity.results as item}<div class="technical-file"><code>{item.relative_path}</code><small>{statusLabels[item.status] ?? item.status} · {item.evidence.reason || item.error || '探测窗口内未发现变化'}</small>{#if item.evidence.open_by_process?.length}<small>打开进程：{item.evidence.open_by_process.map(process => `${process.command || '进程'} (${process.pid})`).join('、')}</small>{/if}{#if item.evidence.matching_lock_ids?.length}<small>匹配锁：{item.evidence.matching_lock_ids.join('、')}</small>{/if}</div>{/each}</details>{/if}
                  {#if impactPreview.plan}<details class="restricted-impact"><summary>计划与文件指纹</summary><pre>{JSON.stringify(impactPreview.plan, null, 2)}</pre></details>{/if}
                  {#if impactPreview.restricted_preview}<div><span>{impactPreview.restricted_preview.trash_package ? '临时材料位置（成功后只留执行凭据）' : impactPreview.restricted_preview.permanent_delete ? '成功后清理临时恢复正文；失败时保留供复核' : '恢复材料保存在源目录之外'}</span><code>{impactPreview.restricted_preview.recovery_path}</code></div><details class="restricted-impact"><summary>原始预览 JSON</summary><pre>{JSON.stringify(impactPreview.restricted_preview, null, 2)}</pre></details>{/if}
                {/if}
              </details>
            </div>
          {/if}
          {#if operationMessage}<div class:operation-message-blocked={Boolean(impactPreview?.blocked || executionMessage && operationMessage === executionMessage)} class="operation-message" role="status">{operationMessage}</div>{/if}

    {/if}
    <OperationWorkflow destructive={operationOptions.trash || operationOptions.permanent_delete} slot="footer" count={impactPreview?.items?.length || (selectedConversation ? 1 : 0)} selected={operationChosen} selectionError={operationSelectionError} previewed={Boolean(impactPreview)} blocked={Boolean(impactPreview?.blocked)} ready={operationExecutable} loading={previewLoading} confirming={executionBusy || destinationBusy} running={operationExecuting} writesEnabled={lifecycleWritesEnabled} needsPassword={operationOptions.encrypted && !operationArchivePassword.trim()} message={impactPreview?.blocked ? '请处理上方原因后重新检查，也可以先暂存操作。' : operationMessage} on:preview={loadImpactPreview} on:confirm={executeOperation} />
  </LibraryDrawer>
  {/if}
  <ExecutionConfirmationDialog archived={executionStrategy.archive} bind:open={executionOpen} challenge={executionChallenge} action={operationSummary(executionStrategy)} sourceRoot={executionSourceRoot} targets={executionTargets} destination={[executionStrategy.archive && executionStrategy.archive_local ? destinationStatus?.archive_path : '', executionStrategy.archive && executionStrategy.archive_remote ? '云端归档：' + (githubArchiveTarget?.full_name || '已选择的私有仓库') : '', executionStrategy.trash ? destinationStatus?.trash_path : '', executionStrategy.permanent_delete ? '.codex 源版本删除，跳过回收站' : ''].filter(Boolean).join(' + ')} permanent={executionStrategy.permanent_delete} restricted={lifecycleRestricted} packagedTrash={executionPackagedTrash} busy={executionBusy} message={executionMessage} on:confirm={(event) => submitExecutionConfirmation(event.detail)} on:refresh={requestExecutionConfirmation} on:cancel={cancelExecutionConfirmation} />
  <DestinationConfirmation bind:open={destinationOpen} status={destinationStatus} busy={destinationBusy} message={destinationMessage} bind:customPath={trashCustomPath} allowCustomPath={trashDetection?.os !== 'windows'} needsArchive={destinationNeedsArchive} needsTrash={destinationNeedsTrash} on:probe={sendTrashProbe} on:found={(event) => confirmDestination(event.detail)} on:savepath={(event) => correctTrashDestination(event.detail)} on:confirmarchive={() => confirmDestination(undefined, true)} on:refresh={() => openDestinationCheck(destinationNeedsArchive, destinationNeedsTrash)} />
</div>
{/if}

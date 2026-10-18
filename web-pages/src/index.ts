// @saker/grayling-pages — view components for the grayling console.
// Phase 1: the simple views (debug, quotas, tokens, audit, segments list,
// experiments list) plus the shared component kit. The shell (web/src)
// provides HostServices via HostProvider; web/src/pages re-exports these
// pages for the router.
export * from './host';
export * from './utils';
export * from './presets';
export * from './lint';
export * from './hooks/useApiData';
export * from './hooks/useMutation';
export * from './hooks/usePolling';
export * from './hooks/useRunAction';
export * from './components/Banner';
export * from './components/Button';
export * from './components/Chip';
export * from './components/ConfirmDialog';
export * from './components/CursorPager';
export * from './components/DataTable';
export * from './components/Dialog';
export * from './components/EmptyState';
export * from './components/ErrorBanner';
export * from './components/FormDialog';
export * from './components/ListSearch';
export * from './components/Pagination';
export * from './components/Skeleton';
export * from './components/Toast';
export * from './components/DiffView';
export * from './components/WorkflowBand';
export * from './components/RevisionsTable';
export * from './components/ReviewPreview';
export * from './components/SegmentMatchProbe';
export * from './components/ApprovalReview';
export * from './editors/EditorDirtyContext';
export * from './editors/FlagEditor';
export * from './editors/SegmentEditor';
export * from './editors/PresetPicker';
export * from './editors/StickyActionBar';
export * from './pages/DebugPage';
export * from './pages/QuotasPage';
export * from './pages/TokensPage';
export * from './pages/AuditPage';
export * from './pages/SegmentsPage';
export * from './pages/ExperimentsPage';
export * from './pages/OverviewPage';
export * from './pages/ApprovalsPage';
export * from './pages/ApprovalDetailPage';
export * from './pages/MetricsPage';
export * from './pages/AiPage';
export * from './pages/FlagDetailPage';
export * from './pages/SegmentDetailPage';
export * from './pages/FlagsPage';
export * from './pages/ExperimentCreatePage';
export * from './pages/ExperimentDetailPage';

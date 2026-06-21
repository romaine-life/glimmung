import { generatePath, matchRoutes, type RouteMatch, type RouteObject } from "react-router-dom";

export type Breadcrumb = {
  label: string;
  to?: string;
};

export const ISSUE_DETAIL_CHILD_ROUTES = {
  summary: "summary",
  runs: "runs",
  run: "runs/:runId",
  runCycle: "runs/:runId/cycles/:cycleId",
  runPhase: "runs/:runId/cycles/:cycleId/phases/:phaseId",
  runJob: "runs/:runId/cycles/:cycleId/phases/:phaseId/jobs/:jobId",
  runStep: "runs/:runId/cycles/:cycleId/phases/:phaseId/jobs/:jobId/steps/:stepId",
  settings: "settings",
  review: "review",
} as const;

const APP_ROUTES = {
  home: "/",
  leases: "/leases/:kind",
  leaseDetail: "/leases/:kind/:leaseId",
  needsAttention: "/needs-attention",
  projects: "/projects",
  newProject: "/projects/new",
  project: "/projects/:project",
  projectLeases: "/projects/:project/leases/:kind",
  projectLeaseDetail: "/projects/:project/leases/:kind/:leaseId",
  projectTestSlot: "/projects/:project/leases/test/slots/:slotId",
  projectWorkflows: "/projects/:project/workflows",
  projectWorkflow: "/projects/:project/workflows/:workflow",
  projectIssues: "/projects/:project/issues",
  newIssue: "/projects/:project/issues/new",
  issue: "/projects/:project/issues/:issueNumber",
  issueRuns: `/projects/:project/issues/:issueNumber/${ISSUE_DETAIL_CHILD_ROUTES.runs}`,
  issueRun: `/projects/:project/issues/:issueNumber/${ISSUE_DETAIL_CHILD_ROUTES.run}`,
  issueRunCycle: `/projects/:project/issues/:issueNumber/${ISSUE_DETAIL_CHILD_ROUTES.runCycle}`,
  issueRunPhase: `/projects/:project/issues/:issueNumber/${ISSUE_DETAIL_CHILD_ROUTES.runPhase}`,
  issueRunJob: `/projects/:project/issues/:issueNumber/${ISSUE_DETAIL_CHILD_ROUTES.runJob}`,
  issueRunStep: `/projects/:project/issues/:issueNumber/${ISSUE_DETAIL_CHILD_ROUTES.runStep}`,
  issueSettings: `/projects/:project/issues/:issueNumber/${ISSUE_DETAIL_CHILD_ROUTES.settings}`,
  issueReview: `/projects/:project/issues/:issueNumber/${ISSUE_DETAIL_CHILD_ROUTES.review}`,
  projectNeedsAttention: "/projects/:project/needs-attention",
  projectRuns: "/projects/:project/runs",
  reviews: "/reviews",
  previews: "/previews",
} as const;

type IssueRunSelectionParams = {
  runId: string;
  cycleId: string;
  phaseId?: string | null;
  jobId?: string | null;
  stepId?: string | null;
};

type BreadcrumbMatch = RouteMatch<string, RouteObject>;

type BreadcrumbHandle = {
  crumb?: (match: BreadcrumbMatch) => Breadcrumb | null;
};

type BreadcrumbRouteObject = {
  path?: string;
  index?: boolean;
  handle?: BreadcrumbHandle;
  children?: BreadcrumbRouteObject[];
};

function param(value: string | number | null | undefined): string {
  return encodeURIComponent(String(value ?? ""));
}

function routePath(pattern: string, params: Record<string, string | number | null | undefined> = {}): string {
  const encoded = Object.fromEntries(Object.entries(params).map(([key, value]) => [key, param(value)]));
  return generatePath(pattern, encoded);
}

export function issueRunSelectionPath(baseUrl: string, selection: IssueRunSelectionParams): string {
  const prefix = baseUrl || "";
  const params = {
    runId: selection.runId,
    cycleId: selection.cycleId,
    phaseId: selection.phaseId ?? undefined,
    jobId: selection.jobId ?? undefined,
    stepId: selection.stepId ?? undefined,
  };
  if (selection.phaseId && selection.jobId && selection.stepId) {
    return `${prefix}/${routePath(ISSUE_DETAIL_CHILD_ROUTES.runStep, params)}`;
  }
  if (selection.phaseId && selection.jobId) {
    return `${prefix}/${routePath(ISSUE_DETAIL_CHILD_ROUTES.runJob, params)}`;
  }
  if (selection.phaseId) {
    return `${prefix}/${routePath(ISSUE_DETAIL_CHILD_ROUTES.runPhase, params)}`;
  }
  return `${prefix}/${routePath(ISSUE_DETAIL_CHILD_ROUTES.runCycle, params)}`;
}

const breadcrumbRoutes: BreadcrumbRouteObject[] = [
  {
    path: APP_ROUTES.home,
    handle: { crumb: () => ({ label: "Home", to: APP_ROUTES.home }) },
    children: [
      { path: "dashboard", handle: { crumb: () => ({ label: "Test leases", to: "/leases/test" }) } },
      {
        path: "leases",
        children: [
          {
            path: ":kind",
            handle: { crumb: (match) => ({ label: leaseKindLabel(match.params.kind), to: routePath(APP_ROUTES.leases, match.params) }) },
            children: [
              { path: ":leaseId", handle: { crumb: (match) => ({ label: `Lease ${match.params.leaseId ?? ""}` }) } },
            ],
          },
        ],
      },
      { path: "needs-attention", handle: { crumb: () => ({ label: "Needs attention" }) } },
      { path: "previews", handle: { crumb: () => ({ label: "Previews", to: APP_ROUTES.previews }) } },
      {
        path: "projects",
        handle: { crumb: () => ({ label: "Projects", to: APP_ROUTES.projects }) },
        children: [
          { path: "new", handle: { crumb: () => ({ label: "New project" }) } },
          {
            path: ":project",
            handle: { crumb: (match) => ({ label: match.params.project ?? "", to: routePath(APP_ROUTES.project, match.params) }) },
            children: [
              {
                path: "leases",
                children: [
                  {
                    path: ":kind",
                    handle: { crumb: (match) => ({ label: leaseKindLabel(match.params.kind), to: routePath(APP_ROUTES.projectLeases, match.params) }) },
                    children: [
                      { path: "slots/:slotId", handle: { crumb: (match) => ({ label: `Slot ${match.params.slotId ?? ""}` }) } },
                      { path: ":leaseId", handle: { crumb: (match) => ({ label: `Lease ${match.params.leaseId ?? ""}` }) } },
                    ],
                  },
                ],
              },
              {
                path: "workflows",
                handle: { crumb: (match) => ({ label: "Workflows", to: routePath(APP_ROUTES.projectWorkflows, match.params) }) },
                children: [
                  { path: ":workflow", handle: { crumb: (match) => ({ label: match.params.workflow ?? "" }) } },
                ],
              },
              {
                path: "issues",
                handle: { crumb: (match) => ({ label: "Issues", to: routePath(APP_ROUTES.projectIssues, match.params) }) },
                children: [
                  { path: "new", handle: { crumb: () => ({ label: "New issue" }) } },
                  {
                    path: ":issueNumber",
                    handle: { crumb: (match) => ({ label: `#${match.params.issueNumber ?? ""}`, to: routePath(APP_ROUTES.issue, match.params) }) },
                    children: [
                      {
                        path: ISSUE_DETAIL_CHILD_ROUTES.summary,
                        handle: { crumb: () => ({ label: "Summary" }) },
                      },
                      {
                        path: "runs",
                        handle: { crumb: (match) => ({ label: "Runs", to: routePath(APP_ROUTES.issueRuns, match.params) }) },
                        children: [
                          {
                            path: ":runId",
                            handle: { crumb: (match) => ({ label: runCrumbLabel(match.params.runId), to: routePath(APP_ROUTES.issueRun, match.params) }) },
                            children: [
                              {
                                path: "cycles/:cycleId",
                                handle: { crumb: (match) => ({ label: cycleCrumbLabel(match.params.cycleId), to: routePath(APP_ROUTES.issueRunCycle, match.params) }) },
                                children: [
                                  {
                                    path: "phases/:phaseId",
                                    handle: { crumb: (match) => ({ label: `phase ${match.params.phaseId ?? ""}`, to: routePath(APP_ROUTES.issueRunPhase, match.params) }) },
                                    children: [
                                      {
                                        path: "jobs/:jobId",
                                        handle: { crumb: (match) => ({ label: `job ${match.params.jobId ?? ""}`, to: routePath(APP_ROUTES.issueRunJob, match.params) }) },
                                        children: [
                                          {
                                            path: "steps/:stepId",
                                            handle: { crumb: (match) => ({ label: `step ${match.params.stepId ?? ""}` }) },
                                          },
                                        ],
                                      },
                                    ],
                                  },
                                ],
                              },
                            ],
                          },
                        ],
                      },
                      {
                        path: ISSUE_DETAIL_CHILD_ROUTES.settings,
                        handle: { crumb: (match) => ({ label: "Settings", to: routePath(APP_ROUTES.issueSettings, match.params) }) },
                      },
                      {
                        path: ISSUE_DETAIL_CHILD_ROUTES.review,
                        handle: { crumb: () => ({ label: "Review" }) },
                      },
                    ],
                  },
                ],
              },
              { path: "needs-attention", handle: { crumb: () => ({ label: "Needs attention" }) } },
              {
                path: "runs",
                handle: { crumb: (match) => ({ label: "Runs", to: routePath(APP_ROUTES.projectRuns, match.params) }) },
              },
            ],
          },
        ],
      },
      { path: "reviews", handle: { crumb: () => ({ label: "Reviews", to: APP_ROUTES.reviews }) } },
      { path: "issues", handle: { crumb: () => ({ label: "Issues", to: "/issues" }) } },
      { path: "runs", handle: { crumb: () => ({ label: "Runs", to: "/runs" }) } },
      { path: "workflows", handle: { crumb: () => ({ label: "Workflows", to: "/workflows" }) } },
      { path: "leases", handle: { crumb: () => ({ label: "Leases", to: "/leases" }) } },
      { path: "test-slots", handle: { crumb: () => ({ label: "Test slots", to: "/test-slots" }) } },
      { path: "admin", handle: { crumb: () => ({ label: "Admin", to: "/admin" }) } },
    ],
  },
];

export function buildBreadcrumbs(pathname: string): Breadcrumb[] {
  const matches = matchRoutes(breadcrumbRoutes as RouteObject[], pathname);
  if (!matches) return fallbackBreadcrumbs(pathname);
  const crumbs = matches
    .map((match) => (match.route.handle as BreadcrumbHandle | undefined)?.crumb?.(match))
    .filter((crumb): crumb is Breadcrumb => Boolean(crumb));
  return crumbs.length > 0 ? crumbs : [{ label: "Home" }];
}

function fallbackBreadcrumbs(pathname: string): Breadcrumb[] {
  const parts = pathname.split("/").filter(Boolean).map(decodeURIComponent);
  if (parts.length === 0) return [{ label: "Home" }];
  return [{ label: "Home", to: "/" }, { label: titleCase(parts[0]) }];
}

function leaseKindLabel(kind: string | undefined): string {
  return kind === "agent" ? "Agent leases" : "Test leases";
}

function runCrumbLabel(runId: string | undefined): string {
  return runId ? `run ${runId}` : "run";
}

function cycleCrumbLabel(cycleId: string | undefined): string {
  return cycleId ? `cycle ${cycleId}` : "cycle";
}

function titleCase(value: string): string {
  return value
    .split("-")
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join(" ");
}

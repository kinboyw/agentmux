import React from "react";
import ReactDOM from "react-dom/client";
import { Panel, PanelGroup, PanelResizeHandle } from "react-resizable-panels";
import {
  Activity,
  Bug,
  ChevronLeft,
  ChevronRight,
  Github,
  Globe,
  Keyboard,
  LayoutGrid,
  LogIn,
  LogOut,
  Monitor,
  Plus,
  Power,
  RefreshCw,
  Search,
  Server,
  Settings,
  ShieldCheck,
  SplitSquareHorizontal,
  SplitSquareVertical,
  Unplug,
  UserPlus,
  UserRound,
  X,
} from "lucide-react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import "./styles.css";
import { Button } from "./components/ui/button";
import { Input } from "./components/ui/input";
import { Select } from "./components/ui/select";
import { Card } from "./components/ui/card";
import { cn, controlDeviceId, makeStreamId, wsBaseFromLocation } from "./lib/utils";

type WorkerView = {
  id: string;
  tenant_id?: string;
  name: string;
  addr: string;
  backend?: string;
  last_seen: string;
  status?: string;
  online?: boolean;
  enabled?: boolean;
  trace_enabled?: boolean;
  debug_enabled?: boolean;
};

type SessionView = {
  id: string;
  tenant_id?: string;
  worker_id: string;
  name: string;
  cwd: string;
  command: string;
  status: string;
  backend?: string;
};

type MainView = "overview" | "workspace";
type SplitDirection = "horizontal" | "vertical";
type DropZone = "left" | "right" | "top" | "bottom" | "center";

type PaneNode = {
  type: "pane";
  id: string;
  sessionId?: string;
};

type SplitNode = {
  type: "split";
  id: string;
  direction: SplitDirection;
  children: LayoutNode[];
};

type LayoutNode = PaneNode | SplitNode;

type WorkspaceTab = {
  id: string;
  title: string;
  layout: LayoutNode;
  activePane: string;
};

type TerminalSettings = {
  tmuxPrefix: string;
};

type PreviewState = {
  loading?: boolean;
  data?: string;
  error?: string;
  scope?: string;
  loadedAt?: number;
};

type Status = {
  tone: "idle" | "ok" | "warn" | "err";
  title: string;
  detail: string;
};

type AuthMode = "login" | "register";

type AuthCredentialPayload = {
  credential: string;
  credential_id: string;
  tenant_id: string;
  device_id: string;
  role?: string;
  expires_at?: string;
  refresh_token?: string;
  refresh_expires_at?: string;
  user?: {
    email: string;
    name: string;
  };
};

type AuthUser = {
  email: string;
  name: string;
};

type WorkerSessionGroup = {
  worker: WorkerView | null;
  workerId: string;
  sessions: SessionView[];
};

type SignalPayload = {
  signal: string;
  signal_id: string;
  tenant_id: string;
  expires_at: string;
  worker_command: string;
  worker_join_command?: string;
  control_command: string;
  control_url: string;
};

type DragPayload =
  | {
      kind: "session";
      sessionId: string;
    }
  | {
      kind: "pane";
      paneId: string;
    };

type DropTarget = {
  paneId: string;
  zone: DropZone;
};

const dragMime = "application/x-agentmux-drag";
const query = new URLSearchParams(window.location.search);
const initialSignal = query.get("signal") || "";
const queryToken = query.get("token") || "";
const initialToken = queryToken || localStorage.getItem("agentmux.token") || "";
const initialTokenExpiresAt = queryToken ? "" : localStorage.getItem("agentmux.token_expires_at") || "";
const initialRefreshToken = queryToken ? "" : localStorage.getItem("agentmux.refresh_token") || "";
const initialRefreshExpiresAt = queryToken ? "" : localStorage.getItem("agentmux.refresh_expires_at") || "";
const initialPaneId: string = crypto.randomUUID();
const initialTabId: string = crypto.randomUUID();
const initialUser = readStoredUser();
const initialTerminalSettings = readTerminalSettings();

function App() {
  const [token, setToken] = React.useState(initialToken);
  const [tokenExpiresAt, setTokenExpiresAt] = React.useState(initialTokenExpiresAt);
  const [refreshToken, setRefreshToken] = React.useState(initialRefreshToken);
  const [refreshExpiresAt, setRefreshExpiresAt] = React.useState(initialRefreshExpiresAt);
  const [workers, setWorkers] = React.useState<WorkerView[]>([]);
  const [sessions, setSessions] = React.useState<SessionView[]>([]);
  const [sidebarOpen, setSidebarOpen] = React.useState(true);
  const [authOpen, setAuthOpen] = React.useState(false);
  const [authMode, setAuthMode] = React.useState<AuthMode>("login");
  const [authForm, setAuthForm] = React.useState({ email: "", password: "", name: "" });
  const [currentUser, setCurrentUser] = React.useState<AuthUser | null>(initialUser);
  const [workerFilter, setWorkerFilter] = React.useState("all");
  const [tokenDraft, setTokenDraft] = React.useState(initialToken);
  const [joinSignal, setJoinSignal] = React.useState<SignalPayload | null>(null);
  const [createOpen, setCreateOpen] = React.useState(false);
  const [joinOpen, setJoinOpen] = React.useState(false);
  const [joinLoading, setJoinLoading] = React.useState(false);
  const [workerSearch, setWorkerSearch] = React.useState("");
  const [sessionSearch, setSessionSearch] = React.useState("");
  const [mainView, setMainView] = React.useState<MainView>("overview");
  const [terminalSettings, setTerminalSettings] = React.useState<TerminalSettings>(initialTerminalSettings);
  const [tabs, setTabs] = React.useState<WorkspaceTab[]>([
    { id: initialTabId, title: "Workspace 1", layout: { type: "pane", id: initialPaneId }, activePane: initialPaneId },
  ]);
  const [activeTabId, setActiveTabId] = React.useState(initialTabId);
  const [previewStates, setPreviewStates] = React.useState<Record<string, PreviewState>>({});
  const [pendingFocusSessionId, setPendingFocusSessionId] = React.useState<string | null>(null);
  const [dropTarget, setDropTarget] = React.useState<DropTarget | null>(null);
  const sessionButtonRefs = React.useRef<Record<string, HTMLButtonElement | null>>({});
  const authRef = React.useRef({
    token: initialToken,
    tokenExpiresAt: initialTokenExpiresAt,
    refreshToken: initialRefreshToken,
    refreshExpiresAt: initialRefreshExpiresAt,
  });
  const refreshInFlight = React.useRef<Promise<string> | null>(null);
  const lastActivityAt = React.useRef(Date.now());
  const [status, setStatus] = React.useState<Status>({
    tone: "idle",
    title: "No session attached",
    detail: "Open a session from the sidebar.",
  });
  const [createForm, setCreateForm] = React.useState({
    worker_id: "",
    name: "demo",
    cwd: ".",
    command: "bash",
  });

  const workerOptions = React.useMemo(
    () => workers.slice().sort((left, right) => workerDisplayLabel(left).localeCompare(workerDisplayLabel(right))),
    [workers],
  );

  const groupedSessions = React.useMemo(
    () => buildWorkerSessionGroups(workerOptions, sessions, workerFilter),
    [workerOptions, sessions, workerFilter],
  );

  const filteredCreateWorkers = React.useMemo(
    () => filterWorkers(workerOptions.filter(workerCanStartSession), workerSearch),
    [workerOptions, workerSearch],
  );

  const visibleSessions = React.useMemo(
    () => filterSessions(sessions, workerOptions, workerFilter, sessionSearch),
    [sessions, workerOptions, workerFilter, sessionSearch],
  );

  const sessionByID = React.useMemo(() => new Map(sessions.map((session) => [session.id, session])), [sessions]);
  const workerByID = React.useMemo(() => new Map(workerOptions.map((worker) => [worker.id, worker])), [workerOptions]);
  const activeTab = React.useMemo(() => tabs.find((tab) => tab.id === activeTabId) || tabs[0], [tabs, activeTabId]);
  const activePane = activeTab.activePane;
  const layout = activeTab.layout;

  React.useEffect(() => {
    authRef.current = { token, tokenExpiresAt, refreshToken, refreshExpiresAt };
  }, [token, tokenExpiresAt, refreshToken, refreshExpiresAt]);

  React.useEffect(() => {
    const markActivity = () => {
      lastActivityAt.current = Date.now();
    };
    window.addEventListener("keydown", markActivity, true);
    window.addEventListener("pointerdown", markActivity, true);
    const timer = window.setInterval(() => {
      if (Date.now() - lastActivityAt.current > 10 * 60 * 1000) return;
      void ensureFreshAccessToken();
    }, 60_000);
    return () => {
      window.removeEventListener("keydown", markActivity, true);
      window.removeEventListener("pointerdown", markActivity, true);
      window.clearInterval(timer);
    };
  }, []);

  React.useEffect(() => {
    if (initialSignal) {
      void exchangeSignal(initialSignal);
    } else if (initialToken) {
      void refreshAll(initialToken);
    }
  }, []);

  React.useEffect(() => {
    if (!createForm.worker_id && workers[0]) {
      const preferred = workers.find(workerCanStartSession) || workers.find(workerIsOnline) || workers[0];
      setCreateForm((form) => ({ ...form, worker_id: preferred.id }));
    }
  }, [workers, createForm.worker_id]);

  React.useEffect(() => {
    if (!pendingFocusSessionId) return;
    const node = sessionButtonRefs.current[pendingFocusSessionId];
    if (!node) return;
    node.scrollIntoView({ block: "nearest" });
    node.focus();
    setPendingFocusSessionId(null);
  }, [pendingFocusSessionId, sessions, workerFilter]);

  async function exchangeSignal(signal: string) {
    setStatus({ tone: "warn", title: "Exchanging signal", detail: "Requesting a browser credential." });
    const res = await fetch("/api/exchange", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        signal,
        role: "control",
        device_id: controlDeviceId(),
        device_name: browserDeviceName(),
      }),
    });
    if (!res.ok) {
      setStatus({ tone: "err", title: "Signal exchange failed", detail: await res.text() });
      return;
    }
    const data = await res.json();
    await acceptCredential(data, "Signal credential ready");
  }

  async function apiFetch(path: string, init: RequestInit = {}, authToken = authRef.current.token) {
    const nextToken = path === "/api/auth/refresh" ? authToken : await ensureFreshAccessToken(authToken);
    const headers = new Headers(init.headers);
    if (nextToken) headers.set("Authorization", `Bearer ${nextToken}`);
    if (init.body && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");
    return fetch(path, { ...init, headers });
  }

  async function refreshAll(authToken = authRef.current.token) {
    const nextToken = await ensureFreshAccessToken(authToken);
    if (nextToken) localStorage.setItem("agentmux.token", nextToken);
    const [workersRes, sessionsRes] = await Promise.all([apiFetch("/api/workers", {}, nextToken), apiFetch("/api/sessions", {}, nextToken)]);
    if (!workersRes.ok || !sessionsRes.ok) {
      setStatus({ tone: "err", title: "Unauthorized or hub unavailable", detail: `${workersRes.status} ${sessionsRes.status}` });
      return;
    }
    const workersPayload = await workersRes.json();
    const sessionsPayload = await sessionsRes.json();
    setWorkers(workersPayload.workers || []);
    setSessions(sessionsPayload.sessions || []);
    setStatus({ tone: "ok", title: "Hub synced", detail: `${workersPayload.workers?.length || 0} workers · ${sessionsPayload.sessions?.length || 0} sessions` });
  }

  async function createSession(event: React.FormEvent) {
    event.preventDefault();
    const targetSessionID = `${createForm.worker_id}/${createForm.name}`;
    const res = await apiFetch("/api/sessions", { method: "POST", body: JSON.stringify(createForm) });
    if (!res.ok) {
      setStatus({ tone: "err", title: "Create failed", detail: await res.text() });
      return;
    }
    setStatus({ tone: "warn", title: "Create queued", detail: `${createForm.worker_id}/${createForm.name}` });
    setCreateOpen(false);
    setWorkerFilter(createForm.worker_id);
    void focusCreatedSession(targetSessionID, createForm.worker_id);
  }

  async function killSession(session: SessionView) {
    const res = await apiFetch(`/api/sessions/${encodeURIComponent(session.worker_id)}/${encodeURIComponent(session.name)}`, {
      method: "DELETE",
    });
    if (!res.ok) {
      setStatus({ tone: "err", title: "Exit failed", detail: await res.text() });
      return;
    }
    setStatus({ tone: "warn", title: "Exit queued", detail: `${session.worker_id}/${session.name}` });
    setTabs((items) => items.map((tab) => ({ ...tab, layout: clearSessionFromLayout(tab.layout, session.id) })));
    window.setTimeout(() => void refreshAll(), 700);
  }

  async function submitAuth(event: React.FormEvent) {
    event.preventDefault();
    const path = authMode === "register" ? "/api/auth/register" : "/api/auth/login";
    const res = await fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        email: authForm.email,
        password: authForm.password,
        name: authForm.name,
        device_id: controlDeviceId(),
        device_name: browserDeviceName(),
      }),
    });
    if (!res.ok) {
      setStatus({ tone: "err", title: authMode === "register" ? "Registration failed" : "Login failed", detail: await res.text() });
      return;
    }
    await acceptCredential(await res.json(), authMode === "register" ? "Account created" : "Signed in");
  }

  async function acceptCredential(data: AuthCredentialPayload, title: string) {
    storeCredential(data);
    localStorage.setItem("agentmux.control_device_id", data.device_id);
    setStatus({
      tone: "ok",
      title,
      detail: `${data.user?.email || data.credential_id} · ${data.tenant_id}`,
    });
    setAuthOpen(false);
    await refreshAll(data.credential);
  }

  async function applyDirectToken() {
    const nextToken = tokenDraft.trim();
    if (!nextToken) {
      setStatus({ tone: "warn", title: "Missing token", detail: "Enter a control credential or dev token." });
      return;
    }
    setToken(nextToken);
    setTokenExpiresAt("");
    setRefreshToken("");
    setRefreshExpiresAt("");
    setCurrentUser(null);
    setJoinSignal(null);
    localStorage.setItem("agentmux.token", nextToken);
    localStorage.removeItem("agentmux.token_expires_at");
    localStorage.removeItem("agentmux.refresh_token");
    localStorage.removeItem("agentmux.refresh_expires_at");
    localStorage.removeItem("agentmux.user");
    setAuthOpen(false);
    await refreshAll(nextToken);
  }

  function signOut() {
    setToken("");
    setTokenDraft("");
    setTokenExpiresAt("");
    setRefreshToken("");
    setRefreshExpiresAt("");
    setCurrentUser(null);
    setJoinSignal(null);
    setWorkers([]);
    setSessions([]);
    authRef.current = { token: "", tokenExpiresAt: "", refreshToken: "", refreshExpiresAt: "" };
    localStorage.removeItem("agentmux.token");
    localStorage.removeItem("agentmux.token_expires_at");
    localStorage.removeItem("agentmux.refresh_token");
    localStorage.removeItem("agentmux.refresh_expires_at");
    localStorage.removeItem("agentmux.user");
    setAuthOpen(false);
    setStatus({ tone: "idle", title: "Signed out", detail: "Sign in or apply a token to control sessions." });
  }

  function storeCredential(data: AuthCredentialPayload) {
    setToken(data.credential);
    setTokenDraft(data.credential);
    setTokenExpiresAt(data.expires_at || "");
    setRefreshToken(data.refresh_token || "");
    setRefreshExpiresAt(data.refresh_expires_at || "");
    authRef.current = {
      token: data.credential,
      tokenExpiresAt: data.expires_at || "",
      refreshToken: data.refresh_token || "",
      refreshExpiresAt: data.refresh_expires_at || "",
    };
    localStorage.setItem("agentmux.token", data.credential);
    setOptionalStorage("agentmux.token_expires_at", data.expires_at);
    setOptionalStorage("agentmux.refresh_token", data.refresh_token);
    setOptionalStorage("agentmux.refresh_expires_at", data.refresh_expires_at);
    if (data.user) {
      setCurrentUser(data.user);
      localStorage.setItem("agentmux.user", JSON.stringify(data.user));
    } else {
      setCurrentUser(null);
      localStorage.removeItem("agentmux.user");
    }
  }

  async function ensureFreshAccessToken(authToken = authRef.current.token) {
    const state = authRef.current;
    if (!authToken) return "";
    if (authToken !== state.token) return state.token || authToken;
    if (!state.refreshToken) return authToken;
    if (!shouldRefreshBrowserToken(state.tokenExpiresAt)) return authToken;
    if (isPast(state.refreshExpiresAt)) {
      clearStoredRefresh();
      return authToken;
    }
    if (!refreshInFlight.current) {
      refreshInFlight.current = refreshBrowserCredential(state.refreshToken).finally(() => {
        refreshInFlight.current = null;
      });
    }
    try {
      return await refreshInFlight.current;
    } catch {
      return authToken;
    }
  }

  async function refreshBrowserCredential(currentRefreshToken: string) {
    const res = await fetch("/api/auth/refresh", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ refresh_token: currentRefreshToken }),
    });
    if (!res.ok) {
      clearStoredRefresh();
      setStatus({ tone: "warn", title: "Session refresh failed", detail: errorDetail(await res.text()) });
      throw new Error("session refresh failed");
    }
    const data = (await res.json()) as AuthCredentialPayload;
    storeCredential(data);
    return data.credential;
  }

  function clearStoredRefresh() {
    setRefreshToken("");
    setRefreshExpiresAt("");
    authRef.current = { ...authRef.current, refreshToken: "", refreshExpiresAt: "" };
    localStorage.removeItem("agentmux.refresh_token");
    localStorage.removeItem("agentmux.refresh_expires_at");
  }

  async function generateJoinSignal() {
    setJoinLoading(true);
    const res = await apiFetch("/api/signals", { method: "POST" });
    if (!res.ok) {
      setJoinLoading(false);
      setStatus({ tone: "err", title: "Signal generation failed", detail: await res.text() });
      return;
    }
    const data = (await res.json()) as SignalPayload;
    setJoinSignal(data);
    setJoinLoading(false);
    setStatus({ tone: "ok", title: "Join signal ready", detail: `${data.tenant_id} · expires ${new Date(data.expires_at).toLocaleString()}` });
  }

  async function openJoinModal() {
    setJoinOpen(true);
    if (!joinSignal && token.trim()) {
      await generateJoinSignal();
    }
  }

  async function focusCreatedSession(sessionID: string, workerID: string) {
    for (let attempt = 0; attempt < 10; attempt++) {
      const res = await apiFetch("/api/sessions");
      if (res.ok) {
        const payload = await res.json();
        const nextSessions = payload.sessions || [];
        setSessions(nextSessions);
        const found = nextSessions.find((session: SessionView) => session.id === sessionID);
        if (found) {
          await attach(found.id);
          setWorkerFilter(workerID);
          setPendingFocusSessionId(found.id);
          setStatus({ tone: "ok", title: "Session ready", detail: found.id });
          return;
        }
      }
      await sleep(350);
    }
    await refreshAll();
  }

  async function startOAuth(provider: "github" | "google") {
    const res = await fetch(`/api/auth/oauth/${provider}?device_id=${encodeURIComponent(controlDeviceId())}`);
    const text = await res.text();
    if (!res.ok) {
      setStatus({ tone: "warn", title: `${provider} OAuth not configured`, detail: errorDetail(text) });
      return;
    }
    const data = JSON.parse(text);
    if (data.url) window.location.href = data.url;
  }

  function setActivePane(paneId: string) {
    setTabs((items) => items.map((tab) => (tab.id === activeTabId ? { ...tab, activePane: paneId } : tab)));
  }

  function setLayout(update: React.SetStateAction<LayoutNode>) {
    setTabs((items) =>
      items.map((tab) => {
        if (tab.id !== activeTabId) return tab;
        const nextLayout = typeof update === "function" ? (update as (node: LayoutNode) => LayoutNode)(tab.layout) : update;
        const nextActivePane = findPane(nextLayout, tab.activePane) ? tab.activePane : firstPaneId(nextLayout);
        return { ...tab, layout: nextLayout, activePane: nextActivePane, title: titleForTab(tab.title, nextLayout) };
      }),
    );
  }

  async function attach(sessionId: string, target: "current" | "new-tab" = "current") {
    await ensureFreshAccessToken();
    setMainView("workspace");
    if (target === "new-tab") {
      const tab = newWorkspaceTab(sessionId, tabTitleForSession(sessionId));
      setTabs((items) => [...items, tab]);
      setActiveTabId(tab.id);
      return;
    }
    setLayout((node) => updatePane(node, activePane, (pane) => ({ ...pane, sessionId })));
  }

  function createWorkspaceTab(sessionId?: string) {
    const tab = newWorkspaceTab(sessionId, sessionId ? tabTitleForSession(sessionId) : `Workspace ${tabs.length + 1}`);
    setTabs((items) => [...items, tab]);
    setActiveTabId(tab.id);
    setMainView("workspace");
  }

  function closeWorkspaceTab(tabId: string) {
    setTabs((items) => {
      if (items.length <= 1) {
        const pane = newPane();
        return [{ ...items[0], title: "Workspace 1", layout: pane, activePane: pane.id }];
      }
      const closingIndex = items.findIndex((tab) => tab.id === tabId);
      const nextItems = items.filter((tab) => tab.id !== tabId);
      if (tabId === activeTabId) {
        const nextIndex = Math.max(0, closingIndex - 1);
        setActiveTabId(nextItems[nextIndex]?.id || nextItems[0].id);
      }
      return nextItems;
    });
  }

  async function loadSessionPreview(session: SessionView, force = false) {
    const current = previewStates[session.id];
    if (!force && (current?.loading || (current?.loadedAt && Date.now() - current.loadedAt < 45_000))) return;
    setPreviewStates((items) => ({ ...items, [session.id]: { ...items[session.id], loading: true, error: "" } }));
    const res = await apiFetch(`/api/sessions/${encodeURIComponent(session.worker_id)}/${encodeURIComponent(session.name)}/preview?lines=22`);
    if (!res.ok) {
      const detail = errorDetailFromResponseText(res.status, await res.text());
      setPreviewStates((items) => ({
        ...items,
        [session.id]: { loading: false, error: detail, loadedAt: Date.now() },
      }));
      setStatus({ tone: "warn", title: "Preview failed", detail: `${session.id} · ${detail}` });
      return;
    }
    const payload = (await res.json()) as { data?: string; scope?: string };
    setPreviewStates((items) => ({
      ...items,
      [session.id]: {
        loading: false,
        data: stripAnsi(payload.data || ""),
        scope: payload.scope || "active_pane",
        loadedAt: Date.now(),
      },
    }));
  }

  async function updateWorker(worker: WorkerView, patch: Partial<Pick<WorkerView, "enabled" | "trace_enabled" | "debug_enabled">>) {
    const res = await apiFetch(`/api/workers/${encodeURIComponent(worker.id)}`, {
      method: "PATCH",
      body: JSON.stringify(patch),
    });
    if (!res.ok) {
      setStatus({ tone: "err", title: "Worker update failed", detail: await res.text() });
      return;
    }
    await refreshAll();
  }

  function splitPaneById(paneId: string, direction: SplitDirection) {
    const pane = newPane();
    setLayout((node) => splitPaneNode(node, paneId, direction, pane).node);
    setActivePane(pane.id);
  }

  function dropOnPane(targetPaneId: string, zone: DropZone, payload: DragPayload) {
    setDropTarget(null);
    if (payload.kind === "session") {
      const pane = zone === "center" ? undefined : newPane(payload.sessionId);
      setLayout((node) => {
        if (zone === "center") return updatePane(node, targetPaneId, (pane) => ({ ...pane, sessionId: payload.sessionId }));
        return insertPaneRelative(node, targetPaneId, zone, pane!).node;
      });
      setActivePane(pane?.id || targetPaneId);
      return;
    }
    if (payload.paneId === targetPaneId) return;
    if (zone === "center") {
      setLayout((node) => swapPaneSessions(node, payload.paneId, targetPaneId));
      setActivePane(targetPaneId);
      return;
    }
    setLayout((node) => {
      const extracted = extractPane(node, payload.paneId);
      if (!extracted.pane || !extracted.node || !findPane(extracted.node, targetPaneId)) return node;
      const inserted = insertPaneRelative(extracted.node, targetPaneId, zone, extracted.pane);
      return inserted.inserted ? inserted.node : node;
    });
    setActivePane(payload.paneId);
  }

  function closePane(id: string) {
    setLayout((node) => {
      const result = removePane(node, id);
      const next = result.node || newPane();
      if (id === activePane || !findPane(next, activePane)) {
        setActivePane(firstPaneId(next));
      }
      return next;
    });
  }

  const activeSessionIds = new Set(
    tabs.flatMap((tab) => collectPanes(tab.layout).map((pane) => pane.sessionId).filter((id): id is string => Boolean(id))),
  );

  return (
    <div className="flex h-screen bg-background text-foreground">
      <aside className={cn("flex h-full shrink-0 flex-col border-r border-border bg-card transition-all", sidebarOpen ? "w-80" : "w-0 overflow-hidden")}>
        <div className="flex h-14 items-center justify-between border-b border-border px-3">
          <div className="flex items-center gap-2 text-sm font-semibold">
            <img src="/agentmux-mark.svg" alt="" className="h-5 w-5 rounded-md" />
            AgentMux
          </div>
          <Button variant="ghost" size="icon" onClick={() => setSidebarOpen(false)} title="Hide sidebar">
            <ChevronLeft className="h-4 w-4" />
          </Button>
        </div>
        <div className="min-h-0 flex-1 overflow-auto p-1.5">
          <div className="mb-2 flex items-center gap-2 px-1">
            <div className="flex-1 text-xs font-medium uppercase text-muted-foreground">Sessions</div>
            <Button variant="ghost" size="icon-sm" onClick={() => void refreshAll()} title="Refresh workers and sessions">
              <RefreshCw className="h-4 w-4" />
            </Button>
          </div>
          <div className="mb-3 px-1">
            <Select value={workerFilter} onChange={(event) => setWorkerFilter(event.target.value)} className="h-8 text-xs">
              <option value="all">All workers</option>
              {workerOptions.map((worker) => (
                <option key={worker.id} value={worker.id}>{workerDisplayLabel(worker)} · {workerStatusLabel(worker)}</option>
              ))}
            </Select>
          </div>
          <div className="space-y-1">
            {sessions.length === 0 ? <div className="px-2 py-4 text-sm text-muted-foreground">No sessions.</div> : null}
            {sessions.length > 0 && groupedSessions.length === 0 ? <div className="px-2 py-4 text-sm text-muted-foreground">No sessions for this worker.</div> : null}
            {groupedSessions.map((group) => (
              <div key={group.workerId} className="space-y-1 pb-2">
                <div className="px-2 pb-1 text-[11px] font-medium uppercase tracking-[0.08em] text-muted-foreground">
                  <div className="flex min-w-0 items-center gap-1.5">
                    <span className={cn("h-1.5 w-1.5 shrink-0 rounded-full", !group.worker || workerIsOnline(group.worker) ? "bg-emerald-500" : "bg-amber-500")} />
                    <div className="truncate">{group.worker ? workerDisplayLabel(group.worker) : group.workerId}</div>
                  </div>
                  <div className="truncate text-[10px] normal-case tracking-normal text-muted-foreground/80">
                    {group.worker ? workerStatusLabel(group.worker) : "unknown"} · {group.worker?.addr || group.workerId} · {group.sessions.length} session{group.sessions.length === 1 ? "" : "s"}
                  </div>
                </div>
                {group.sessions.map((session) => (
                  <div
                    key={session.id}
                    className={cn(
                      "rounded-md border border-transparent hover:border-border hover:bg-secondary",
                      activeSessionIds.has(session.id) && "border-primary/40 bg-primary/10",
                    )}
                  >
                    <div className="flex items-start gap-1">
                      <button
                        ref={(node) => {
                          sessionButtonRefs.current[session.id] = node;
                        }}
                        type="button"
                        draggable
                        className="min-w-0 flex-1 px-2 py-1.5 text-left"
                        onClick={() => void attach(session.id)}
                        onDragStart={(event) => setDragPayload(event, { kind: "session", sessionId: session.id })}
                        onDragEnd={() => setDropTarget(null)}
                      >
                        <div className="flex min-w-0 items-center gap-1.5">
                          <div className="truncate text-sm font-medium">{session.name || session.id}</div>
                          <BackendBadge value={sessionBackendLabel(session, group.worker)} />
                        </div>
                        <div className="truncate text-xs text-muted-foreground">{session.command || "shell"} · {session.status || "unknown"}</div>
                        <div className="truncate text-xs text-muted-foreground">{session.cwd}</div>
                      </button>
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        className="mt-1 mr-1 shrink-0"
                        onClick={() => void killSession(session)}
                        title="Exit session"
                      >
                        <Power className="h-4 w-4" />
                      </Button>
                    </div>
                  </div>
                ))}
              </div>
            ))}
          </div>
        </div>
        <div className="grid grid-cols-2 gap-2 border-t border-border p-3">
          <Button variant="secondary" onClick={() => setCreateOpen(true)}>
            <Plus className="h-4 w-4" />
            Create
          </Button>
          <Button variant="secondary" onClick={() => void openJoinModal()}>
            <UserPlus className="h-4 w-4" />
            Join
          </Button>
        </div>
      </aside>

      <main className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-11 items-center justify-between border-b border-border bg-card px-2">
          <div className="flex min-w-0 items-center gap-2">
            {!sidebarOpen ? (
              <Button variant="ghost" size="icon-sm" onClick={() => setSidebarOpen(true)} title="Show sidebar">
                <ChevronRight className="h-4 w-4" />
              </Button>
            ) : null}
            <div className="flex items-center gap-1 rounded-md border border-border bg-background p-0.5">
              <Button variant={mainView === "overview" ? "secondary" : "ghost"} size="xs" onClick={() => setMainView("overview")}>
                <Server className="h-3.5 w-3.5" />
                Overview
              </Button>
              <Button variant={mainView === "workspace" ? "secondary" : "ghost"} size="xs" onClick={() => setMainView("workspace")}>
                <LayoutGrid className="h-3.5 w-3.5" />
                Workspace
              </Button>
            </div>
            <div className="min-w-0">
              <div className="truncate text-sm font-medium">{status.title}</div>
              <div className="truncate text-xs text-muted-foreground">{status.detail}</div>
            </div>
          </div>
          <div className="flex items-center gap-2">
            <div className="hidden max-w-[280px] min-w-0 items-center gap-2 rounded-md border border-border bg-background px-2 py-1 sm:flex">
              <div className="grid h-6 w-6 shrink-0 place-items-center rounded-md bg-primary/10 text-primary">
                <UserRound className="h-3.5 w-3.5" />
              </div>
              <div className="min-w-0">
                <div className="truncate text-xs font-medium">{currentUser?.name || "Guest control"}</div>
                <div className="truncate text-[11px] text-muted-foreground">{currentUser?.email || "Use sign in or direct token access."}</div>
              </div>
            </div>
            <Button variant="secondary" size="sm" onClick={() => setAuthOpen(true)}>
              {currentUser ? <UserRound className="h-4 w-4" /> : <LogIn className="h-4 w-4" />}
              {currentUser ? "Account" : "Sign in"}
            </Button>
            <span className={cn("h-2 w-2 rounded-full", status.tone === "ok" && "bg-emerald-500", status.tone === "warn" && "bg-amber-500", status.tone === "err" && "bg-red-500", status.tone === "idle" && "bg-muted-foreground")} />
          </div>
        </header>
        <div className="min-h-0 flex-1">
          {mainView === "overview" ? (
            <OverviewPage
              workers={workerOptions}
              sessions={visibleSessions}
              allSessions={sessions}
              workerFilter={workerFilter}
              sessionSearch={sessionSearch}
              activeSessionIds={activeSessionIds}
              previewStates={previewStates}
              onWorkerFilterChange={setWorkerFilter}
              onSessionSearchChange={setSessionSearch}
              onAttach={(session) => void attach(session.id)}
              onAttachNewTab={(session) => void attach(session.id, "new-tab")}
              onLoadPreview={(session, force) => void loadSessionPreview(session, force)}
              onKillSession={(session) => void killSession(session)}
              onUpdateWorker={(worker, patch) => void updateWorker(worker, patch)}
            />
          ) : (
            <WorkspaceView
              tabs={tabs}
              activeTabId={activeTabId}
              layout={layout}
              activePane={activePane}
              token={token}
              dropTarget={dropTarget}
              onActiveTabChange={setActiveTabId}
              onCreateTab={() => createWorkspaceTab()}
              onCloseTab={closeWorkspaceTab}
              onFocusPane={setActivePane}
              onSplitPane={splitPaneById}
              onClosePane={closePane}
              onDropTarget={setDropTarget}
              onDropPayload={dropOnPane}
              setStatus={setStatus}
            />
          )}
        </div>
      </main>
      <AuthModal
        open={authOpen}
        authMode={authMode}
        authForm={authForm}
        currentUser={currentUser}
        token={tokenDraft}
        onClose={() => setAuthOpen(false)}
        onModeChange={setAuthMode}
        onFormChange={setAuthForm}
        onTokenChange={setTokenDraft}
        onSubmit={submitAuth}
        onApplyDirectToken={() => void applyDirectToken()}
        onOAuth={(provider) => void startOAuth(provider)}
        onLogout={signOut}
      />
      <CreateSessionModal
        open={createOpen}
        createForm={createForm}
        workerSearch={workerSearch}
        workers={filteredCreateWorkers}
        onClose={() => setCreateOpen(false)}
        onSubmit={createSession}
        onWorkerSearchChange={setWorkerSearch}
        onFormChange={setCreateForm}
      />
      <JoinSignalModal
        open={joinOpen}
        joinSignal={joinSignal}
        loading={joinLoading}
        tokenReady={!!token.trim()}
        onClose={() => setJoinOpen(false)}
        onGenerate={() => void generateJoinSignal()}
      />
    </div>
  );
}

function OverviewPage({
  workers,
  sessions,
  allSessions,
  workerFilter,
  sessionSearch,
  activeSessionIds,
  previewStates,
  onWorkerFilterChange,
  onSessionSearchChange,
  onAttach,
  onAttachNewTab,
  onLoadPreview,
  onKillSession,
  onUpdateWorker,
}: {
  workers: WorkerView[];
  sessions: SessionView[];
  allSessions: SessionView[];
  workerFilter: string;
  sessionSearch: string;
  activeSessionIds: Set<string>;
  previewStates: Record<string, PreviewState>;
  onWorkerFilterChange: (workerID: string) => void;
  onSessionSearchChange: (query: string) => void;
  onAttach: (session: SessionView) => void;
  onAttachNewTab: (session: SessionView) => void;
  onLoadPreview: (session: SessionView, force?: boolean) => void;
  onKillSession: (session: SessionView) => void;
  onUpdateWorker: (worker: WorkerView, patch: Partial<Pick<WorkerView, "enabled" | "trace_enabled" | "debug_enabled">>) => void;
}) {
  const workerByID = React.useMemo(() => new Map(workers.map((worker) => [worker.id, worker])), [workers]);
  const onlineWorkers = workers.filter(workerIsOnline).length;
  const enabledWorkers = workers.filter(workerEnabled).length;
  const previewSessions = sessions.slice(0, 80);

  return (
    <div className="h-full overflow-auto bg-background">
      <div className="space-y-4 p-4">
        <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-4">
          <MetricTile label="Workers" value={`${onlineWorkers}/${workers.length}`} detail={`${enabledWorkers} enabled`} />
          <MetricTile label="Sessions" value={`${allSessions.length}`} detail={`${previewSessions.length} visible`} />
          <MetricTile label="Attached" value={`${activeSessionIds.size}`} detail="workspace panes and tabs" />
          <MetricTile label="Preview" value="active pane" detail="attach opens the whole session" />
        </div>

        <div className="grid gap-4 xl:grid-cols-[360px_minmax(0,1fr)]">
          <section className="min-w-0 space-y-2">
            <div className="flex items-center justify-between gap-2">
              <div>
                <div className="text-sm font-semibold">Workers</div>
                <div className="text-xs text-muted-foreground">Status, access and diagnostics</div>
              </div>
              <Button variant="ghost" size="xs" onClick={() => onWorkerFilterChange("all")}>
                All
              </Button>
            </div>
            <div className="space-y-2">
              {workers.length === 0 ? <EmptyState title="No workers" detail="Generate a join command from the sidebar." /> : null}
              {workers.map((worker) => (
                <WorkerCard
                  key={worker.id}
                  worker={worker}
                  selected={workerFilter === worker.id}
                  sessionCount={allSessions.filter((session) => session.worker_id === worker.id).length}
                  onSelect={() => onWorkerFilterChange(worker.id)}
                  onUpdate={(patch) => onUpdateWorker(worker, patch)}
                />
              ))}
            </div>
          </section>

          <section className="min-w-0 space-y-2">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div>
                <div className="text-sm font-semibold">Session Preview</div>
                <div className="text-xs text-muted-foreground">Preview shows active pane output; attach opens the full session.</div>
              </div>
              <div className="flex min-w-[320px] flex-1 flex-wrap justify-end gap-2">
                <Select value={workerFilter} onChange={(event) => onWorkerFilterChange(event.target.value)} className="h-8 max-w-[220px] text-xs">
                  <option value="all">All workers</option>
                  {workers.map((worker) => (
                    <option key={worker.id} value={worker.id}>{workerDisplayLabel(worker)} · {workerStatusLabel(worker)}</option>
                  ))}
                </Select>
                <div className="relative min-w-[220px] flex-1 sm:max-w-xs">
                  <Search className="pointer-events-none absolute left-2.5 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
                  <Input
                    value={sessionSearch}
                    onChange={(event) => onSessionSearchChange(event.target.value)}
                    placeholder="Filter sessions"
                    className="h-8 pl-8 text-xs"
                  />
                </div>
              </div>
            </div>
            {previewSessions.length === 0 ? (
              <EmptyState title="No sessions" detail="Create a session or adjust the current filter." />
            ) : (
              <div className="grid gap-2 md:grid-cols-2 2xl:grid-cols-3">
                {previewSessions.map((session) => (
                  <SessionPreviewCard
                    key={session.id}
                    session={session}
                    worker={workerByID.get(session.worker_id) || null}
                    active={activeSessionIds.has(session.id)}
                    preview={previewStates[session.id]}
                    onAttach={() => onAttach(session)}
                    onAttachNewTab={() => onAttachNewTab(session)}
                    onLoadPreview={(force) => onLoadPreview(session, force)}
                    onKill={() => onKillSession(session)}
                  />
                ))}
              </div>
            )}
          </section>
        </div>
      </div>
    </div>
  );
}

function MetricTile({ label, value, detail }: { label: string; value: string; detail: string }) {
  return (
    <div className="rounded-md border border-border bg-card px-3 py-2">
      <div className="text-[11px] font-medium uppercase text-muted-foreground">{label}</div>
      <div className="mt-1 text-lg font-semibold">{value}</div>
      <div className="truncate text-xs text-muted-foreground">{detail}</div>
    </div>
  );
}

function WorkerCard({
  worker,
  selected,
  sessionCount,
  onSelect,
  onUpdate,
}: {
  worker: WorkerView;
  selected: boolean;
  sessionCount: number;
  onSelect: () => void;
  onUpdate: (patch: Partial<Pick<WorkerView, "enabled" | "trace_enabled" | "debug_enabled">>) => void;
}) {
  const enabled = workerEnabled(worker);
  const online = workerIsOnline(worker);
  return (
    <Card className={cn("space-y-3 bg-card p-3 transition-colors", selected && "border-primary/50 bg-primary/10")}>
      <button type="button" className="flex w-full min-w-0 items-start gap-2 text-left" onClick={onSelect}>
        <div className={cn("grid h-8 w-8 shrink-0 place-items-center rounded-md", online ? "bg-emerald-500/15 text-emerald-300" : "bg-amber-500/15 text-amber-300")}>
          <Server className="h-4 w-4" />
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex min-w-0 items-center gap-1.5">
            <div className="truncate text-sm font-semibold">{workerDisplayLabel(worker)}</div>
            <BackendBadge value={worker.backend} />
          </div>
          <div className="truncate text-xs text-muted-foreground">{worker.addr || worker.id}</div>
          <div className="mt-1 flex flex-wrap items-center gap-1.5">
            <StatusBadge tone={online ? "ok" : "warn"}>{workerStatusLabel(worker)}</StatusBadge>
            <StatusBadge tone={enabled ? "ok" : "err"}>{enabled ? "enabled" : "disabled"}</StatusBadge>
            <StatusBadge>{sessionCount} sessions</StatusBadge>
          </div>
        </div>
      </button>
      <div className="grid grid-cols-3 gap-1">
        <Button variant={enabled ? "secondary" : "ghost"} size="xs" type="button" onClick={() => onUpdate({ enabled: !enabled })}>
          <ShieldCheck className="h-3.5 w-3.5" />
          {enabled ? "Disable" : "Enable"}
        </Button>
        <Button variant={worker.trace_enabled ? "secondary" : "ghost"} size="xs" type="button" onClick={() => onUpdate({ trace_enabled: !worker.trace_enabled })}>
          <Activity className="h-3.5 w-3.5" />
          Trace
        </Button>
        <Button variant={worker.debug_enabled ? "secondary" : "ghost"} size="xs" type="button" onClick={() => onUpdate({ debug_enabled: !worker.debug_enabled })}>
          <Bug className="h-3.5 w-3.5" />
          Debug
        </Button>
      </div>
      <div className="text-[11px] text-muted-foreground">Last seen {formatRelativeTime(worker.last_seen)}</div>
    </Card>
  );
}

function SessionPreviewCard({
  session,
  worker,
  active,
  preview,
  onAttach,
  onAttachNewTab,
  onLoadPreview,
  onKill,
}: {
  session: SessionView;
  worker: WorkerView | null;
  active: boolean;
  preview?: PreviewState;
  onAttach: () => void;
  onAttachNewTab: () => void;
  onLoadPreview: (force?: boolean) => void;
  onKill: () => void;
}) {
  React.useEffect(() => {
    onLoadPreview(false);
  }, [session.id]);

  const previewText = preview?.data?.trimEnd() || "";
  return (
    <Card
      className={cn("group flex min-h-[260px] flex-col overflow-hidden bg-card transition-colors", active && "border-primary/50 bg-primary/10")}
      draggable
      onDragStart={(event) => setDragPayload(event, { kind: "session", sessionId: session.id })}
    >
      <div className="space-y-2 border-b border-border px-3 py-2">
        <div className="flex items-start justify-between gap-2">
          <button type="button" className="min-w-0 text-left" onClick={onAttach}>
            <div className="flex min-w-0 items-center gap-1.5">
              <div className="truncate text-sm font-semibold">{session.name || session.id}</div>
              <BackendBadge value={sessionBackendLabel(session, worker)} />
            </div>
            <div className="truncate text-xs text-muted-foreground">{worker ? workerDisplayLabel(worker) : session.worker_id}</div>
          </button>
          <StatusBadge tone={active ? "ok" : undefined}>{active ? "attached" : session.status || "idle"}</StatusBadge>
        </div>
        <div className="truncate text-xs text-muted-foreground">{session.command || "shell"} · {session.cwd || "."}</div>
      </div>
      <button type="button" className="min-h-0 flex-1 bg-[#050607] p-3 text-left" onClick={onAttach}>
        <div className="mb-2 flex items-center justify-between text-[11px] text-muted-foreground">
          <span>{preview?.scope || "active_pane"}</span>
          <span>{preview?.loadedAt ? formatRelativeTime(new Date(preview.loadedAt).toISOString()) : "not loaded"}</span>
        </div>
        <pre className="h-32 overflow-hidden whitespace-pre-wrap break-words font-mono text-[11px] leading-4 text-[#d7e2df]">
          {preview?.loading ? "Loading active pane preview..." : preview?.error ? `Preview unavailable: ${preview.error}` : previewText || "No active pane output yet."}
        </pre>
      </button>
      <div className="flex items-center justify-between gap-1 border-t border-border px-2 py-2">
        <div className="flex items-center gap-1">
          <Button variant="secondary" size="xs" type="button" onClick={onAttach}>
            <Monitor className="h-3.5 w-3.5" />
            Attach
          </Button>
          <Button variant="ghost" size="xs" type="button" onClick={onAttachNewTab}>
            <Plus className="h-3.5 w-3.5" />
            Tab
          </Button>
        </div>
        <div className="flex items-center gap-1">
          <Button variant="ghost" size="icon-sm" type="button" onClick={() => onLoadPreview(true)} title="Refresh active pane preview">
            <Eye className="h-4 w-4" />
          </Button>
          <Button variant="ghost" size="icon-sm" type="button" onClick={onKill} title="Exit session">
            <Power className="h-4 w-4" />
          </Button>
        </div>
      </div>
    </Card>
  );
}

function WorkspaceView({
  tabs,
  activeTabId,
  layout,
  activePane,
  token,
  dropTarget,
  onActiveTabChange,
  onCreateTab,
  onCloseTab,
  onFocusPane,
  onSplitPane,
  onClosePane,
  onDropTarget,
  onDropPayload,
  setStatus,
}: {
  tabs: WorkspaceTab[];
  activeTabId: string;
  layout: LayoutNode;
  activePane: string;
  token: string;
  dropTarget: DropTarget | null;
  onActiveTabChange: (tabID: string) => void;
  onCreateTab: () => void;
  onCloseTab: (tabID: string) => void;
  onFocusPane: (id: string) => void;
  onSplitPane: (paneId: string, direction: SplitDirection) => void;
  onClosePane: (id: string) => void;
  onDropTarget: (target: DropTarget | null) => void;
  onDropPayload: (paneId: string, zone: DropZone, payload: DragPayload) => void;
  setStatus: React.Dispatch<React.SetStateAction<Status>>;
}) {
  return (
    <div className="flex h-full min-h-0 flex-col bg-background">
      <div className="flex h-9 items-center gap-2 border-b border-border bg-card px-2">
        <Monitor className="h-4 w-4 shrink-0 text-muted-foreground" />
        <div className="flex min-w-0 flex-1 items-center gap-1 overflow-x-auto">
          {tabs.map((tab) => (
            <button
              key={tab.id}
              type="button"
              className={cn(
                "flex h-7 max-w-[220px] shrink-0 items-center gap-1 rounded-md border px-2 text-xs transition-colors",
                tab.id === activeTabId ? "border-primary/50 bg-primary/10 text-foreground" : "border-transparent text-muted-foreground hover:bg-secondary hover:text-foreground",
              )}
              onClick={() => onActiveTabChange(tab.id)}
            >
              <span className="truncate">{tab.title}</span>
              {tabs.length > 1 ? (
                <span
                  role="button"
                  tabIndex={0}
                  className="grid h-4 w-4 place-items-center rounded hover:bg-secondary"
                  onClick={(event) => {
                    event.stopPropagation();
                    onCloseTab(tab.id);
                  }}
                >
                  <X className="h-3 w-3" />
                </span>
              ) : null}
            </button>
          ))}
        </div>
        <Button variant="ghost" size="icon-sm" onClick={onCreateTab} title="New workspace tab">
          <Plus className="h-4 w-4" />
        </Button>
      </div>
      <div className="min-h-0 flex-1">
        <LayoutRenderer
          node={layout}
          activePane={activePane}
          token={token}
          onFocusPane={onFocusPane}
          onSplitPane={onSplitPane}
          onClosePane={onClosePane}
          dropTarget={dropTarget}
          onDropTarget={onDropTarget}
          onDropPayload={onDropPayload}
          setStatus={setStatus}
        />
      </div>
    </div>
  );
}

function BackendBadge({ value }: { value?: string }) {
  return <StatusBadge>{(value || "backend").toLowerCase()}</StatusBadge>;
}

function StatusBadge({ tone, children }: { tone?: "ok" | "warn" | "err"; children: React.ReactNode }) {
  return (
    <span
      className={cn(
        "inline-flex h-5 shrink-0 items-center rounded border px-1.5 text-[10px] font-medium uppercase tracking-[0.04em]",
        tone === "ok" && "border-emerald-500/30 bg-emerald-500/10 text-emerald-300",
        tone === "warn" && "border-amber-500/30 bg-amber-500/10 text-amber-300",
        tone === "err" && "border-red-500/30 bg-red-500/10 text-red-300",
        !tone && "border-border bg-secondary text-muted-foreground",
      )}
    >
      {children}
    </span>
  );
}

function EmptyState({ title, detail }: { title: string; detail: string }) {
  return (
    <div className="rounded-md border border-dashed border-border bg-card px-3 py-8 text-center">
      <div className="text-sm font-medium">{title}</div>
      <div className="mt-1 text-xs text-muted-foreground">{detail}</div>
    </div>
  );
}

function AuthModal({
  open,
  authMode,
  authForm,
  currentUser,
  token,
  onClose,
  onModeChange,
  onFormChange,
  onTokenChange,
  onSubmit,
  onApplyDirectToken,
  onOAuth,
  onLogout,
}: {
  open: boolean;
  authMode: AuthMode;
  authForm: { email: string; password: string; name: string };
  currentUser: AuthUser | null;
  token: string;
  onClose: () => void;
  onModeChange: (mode: AuthMode) => void;
  onFormChange: (form: { email: string; password: string; name: string }) => void;
  onTokenChange: (token: string) => void;
  onSubmit: (event: React.FormEvent) => void;
  onApplyDirectToken: () => void;
  onOAuth: (provider: "github" | "google") => void;
  onLogout: () => void;
}) {
  if (!open) return null;

  return (
    <div className="absolute inset-0 z-50 flex items-center justify-center bg-black/60 p-4 backdrop-blur-sm">
      <Card className="w-full max-w-md bg-card/95 shadow-2xl">
        <div className="flex items-center justify-between border-b border-border px-4 py-3">
          <div>
            <div className="text-sm font-semibold">Control access</div>
            <div className="text-xs text-muted-foreground">
              {currentUser ? `${currentUser.name} · ${currentUser.email}` : "Register or sign in to sync browser access."}
            </div>
          </div>
          <Button variant="ghost" size="icon-sm" onClick={onClose} title="Close">
            <X className="h-4 w-4" />
          </Button>
        </div>
        <div className="space-y-4 p-4">
          {currentUser ? (
            <div className="space-y-4">
              <div className="rounded-md border border-border bg-background/80 p-3">
                <div className="mb-2 flex items-center gap-2">
                  <div className="grid h-9 w-9 place-items-center rounded-md bg-primary/10 text-primary">
                    <UserRound className="h-4 w-4" />
                  </div>
                  <div className="min-w-0">
                    <div className="truncate text-sm font-medium">{currentUser.name || currentUser.email}</div>
                    <div className="truncate text-xs text-muted-foreground">{currentUser.email}</div>
                  </div>
                </div>
                <div className="truncate text-xs text-muted-foreground">Browser access is active on this Hub.</div>
              </div>
              <Button variant="secondary" className="w-full" type="button" onClick={onLogout}>
                <LogOut className="h-4 w-4" />
                Sign out
              </Button>
            </div>
          ) : (
            <>
              <div className="grid grid-cols-2 rounded-md bg-secondary p-1">
                <Button variant={authMode === "login" ? "default" : "ghost"} size="sm" onClick={() => onModeChange("login")} type="button">
                  <LogIn className="h-4 w-4" />
                  Sign in
                </Button>
                <Button variant={authMode === "register" ? "default" : "ghost"} size="sm" onClick={() => onModeChange("register")} type="button">
                  <UserPlus className="h-4 w-4" />
                  Register
                </Button>
              </div>
              <form className="space-y-2" onSubmit={onSubmit}>
                <Input type="email" value={authForm.email} onChange={(event) => onFormChange({ ...authForm, email: event.target.value })} placeholder="email" autoComplete="email" />
                {authMode === "register" ? (
                  <Input value={authForm.name} onChange={(event) => onFormChange({ ...authForm, name: event.target.value })} placeholder="display name" autoComplete="name" />
                ) : null}
                <Input type="password" value={authForm.password} onChange={(event) => onFormChange({ ...authForm, password: event.target.value })} placeholder="password" autoComplete={authMode === "register" ? "new-password" : "current-password"} />
                <Button variant="secondary" className="w-full" type="submit">
                  {authMode === "register" ? <UserPlus className="h-4 w-4" /> : <LogIn className="h-4 w-4" />}
                  {authMode === "register" ? "Create account" : "Sign in"}
                </Button>
              </form>
              <div className="grid grid-cols-2 gap-2">
                <Button variant="ghost" size="sm" type="button" onClick={() => onOAuth("github")}>
                  <Github className="h-4 w-4" />
                  GitHub
                </Button>
                <Button variant="ghost" size="sm" type="button" onClick={() => onOAuth("google")}>
                  <Globe className="h-4 w-4" />
                  Google
                </Button>
              </div>
              <div className="space-y-2 rounded-md border border-border bg-background/80 p-3">
                <div className="text-xs font-medium uppercase text-muted-foreground">Direct token</div>
                <Input value={token} onChange={(event) => onTokenChange(event.target.value)} placeholder="amx_cred_... or dev token" spellCheck={false} />
                <Button variant="secondary" className="w-full" type="button" onClick={onApplyDirectToken}>
                  <RefreshCw className="h-4 w-4" />
                  Use token
                </Button>
              </div>
            </>
          )}
        </div>
      </Card>
    </div>
  );
}

function SignalCommand({ title, value, mono = true }: { title: string; value: string; mono?: boolean }) {
  return (
    <div className="space-y-1">
      <div className="text-[11px] font-medium uppercase text-muted-foreground">{title}</div>
      <div className={cn("rounded-md border border-border bg-card px-2 py-2 text-[11px] text-foreground", mono && "font-mono")}>
        {value}
      </div>
    </div>
  );
}

function JoinSignalModal({
  open,
  joinSignal,
  loading,
  tokenReady,
  onClose,
  onGenerate,
}: {
  open: boolean;
  joinSignal: SignalPayload | null;
  loading: boolean;
  tokenReady: boolean;
  onClose: () => void;
  onGenerate: () => void;
}) {
  if (!open) return null;

  return (
    <div className="absolute inset-0 z-50 flex items-center justify-center bg-black/60 p-4 backdrop-blur-sm">
      <Card className="w-full max-w-2xl bg-card/95 shadow-2xl">
        <div className="flex items-center justify-between border-b border-border px-4 py-3">
          <div>
            <div className="text-sm font-semibold">Join Worker</div>
            <div className="text-xs text-muted-foreground">Generate a tenant-scoped signal and copy Worker commands for this Hub.</div>
          </div>
          <Button variant="ghost" size="icon-sm" onClick={onClose} title="Close">
            <X className="h-4 w-4" />
          </Button>
        </div>
        <div className="space-y-4 p-4">
          <div className="flex items-center justify-between gap-3">
            <div className="text-xs text-muted-foreground">
              {tokenReady ? "Use the install command on a fresh machine, or the direct join command when agentmux is already installed. Control devices sign in separately." : "Sign in or apply a control token before generating worker join commands."}
            </div>
            <Button variant="secondary" size="sm" type="button" onClick={onGenerate} disabled={!tokenReady || loading}>
              <UserPlus className="h-4 w-4" />
              {loading ? "Generating..." : "Generate"}
            </Button>
          </div>
          {joinSignal ? (
            <div className="grid gap-3 md:grid-cols-2">
              <SignalCommand title="Signal" value={joinSignal.signal} mono={false} />
              <SignalCommand title="Install and join Worker" value={joinSignal.worker_command} />
              <SignalCommand title="Installed Worker join" value={joinSignal.worker_join_command || `agentmux worker join --hub ${wsBaseFromLocation()} --join ${joinSignal.signal} --name "$(hostname)"`} />
              <SignalCommand title="Web Control login" value={joinSignal.control_url} mono={false} />
              <SignalCommand title="TUI Control" value={joinSignal.control_command || `agentmux-tui --hub ${window.location.origin}`} />
              <div className="md:col-span-2 text-[11px] text-muted-foreground">
                Tenant {joinSignal.tenant_id} · expires {new Date(joinSignal.expires_at).toLocaleString()}
              </div>
            </div>
          ) : (
            <div className="rounded-md border border-dashed border-border bg-background/70 px-3 py-8 text-center text-sm text-muted-foreground">
              {loading ? "Generating join commands..." : "No join signal yet."}
            </div>
          )}
        </div>
      </Card>
    </div>
  );
}

function CreateSessionModal({
  open,
  createForm,
  workerSearch,
  workers,
  onClose,
  onSubmit,
  onWorkerSearchChange,
  onFormChange,
}: {
  open: boolean;
  createForm: { worker_id: string; name: string; cwd: string; command: string };
  workerSearch: string;
  workers: WorkerView[];
  onClose: () => void;
  onSubmit: (event: React.FormEvent) => void;
  onWorkerSearchChange: (value: string) => void;
  onFormChange: React.Dispatch<React.SetStateAction<{ worker_id: string; name: string; cwd: string; command: string }>>;
}) {
  if (!open) return null;

  return (
    <div className="absolute inset-0 z-50 flex items-center justify-center bg-black/60 p-4 backdrop-blur-sm">
      <Card className="w-full max-w-md bg-card/95 shadow-2xl">
        <div className="flex items-center justify-between border-b border-border px-4 py-3">
          <div>
            <div className="text-sm font-semibold">Create session</div>
            <div className="text-xs text-muted-foreground">Launch a new shell or agent process on a selected worker.</div>
          </div>
          <Button variant="ghost" size="icon-sm" onClick={onClose} title="Close">
            <X className="h-4 w-4" />
          </Button>
        </div>
        <form className="space-y-3 p-4" onSubmit={onSubmit}>
          <div className="space-y-2">
            <div className="text-xs font-medium uppercase text-muted-foreground">Worker</div>
            <div className="relative">
              <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
              <Input
                value={workerSearch}
                onChange={(event) => onWorkerSearchChange(event.target.value)}
                placeholder="Filter workers"
                className="pl-9"
              />
            </div>
            <Select value={createForm.worker_id} onChange={(event) => onFormChange((form) => ({ ...form, worker_id: event.target.value }))}>
              <option value="">Select worker</option>
              {workers.map((worker) => (
                <option key={worker.id} value={worker.id}>{workerDisplayLabel(worker)} · {workerStatusLabel(worker)}</option>
              ))}
            </Select>
            {workers.length === 0 ? <div className="text-xs text-muted-foreground">No workers matched the current filter.</div> : null}
          </div>
          <Input value={createForm.name} onChange={(event) => onFormChange((form) => ({ ...form, name: event.target.value }))} placeholder="session name" />
          <Input value={createForm.cwd} onChange={(event) => onFormChange((form) => ({ ...form, cwd: event.target.value }))} placeholder="working directory" />
          <Input value={createForm.command} onChange={(event) => onFormChange((form) => ({ ...form, command: event.target.value }))} placeholder="command" />
          <div className="flex justify-end gap-2">
            <Button variant="ghost" type="button" onClick={onClose}>Cancel</Button>
            <Button type="submit">
              <Plus className="h-4 w-4" />
              Create Session
            </Button>
          </div>
        </form>
      </Card>
    </div>
  );
}

function LayoutRenderer({
  node,
  activePane,
  token,
  onFocusPane,
  onSplitPane,
  onClosePane,
  dropTarget,
  onDropTarget,
  onDropPayload,
  setStatus,
}: {
  node: LayoutNode;
  activePane: string;
  token: string;
  onFocusPane: (id: string) => void;
  onSplitPane: (paneId: string, direction: SplitDirection) => void;
  onClosePane: (id: string) => void;
  dropTarget: DropTarget | null;
  onDropTarget: (target: DropTarget | null) => void;
  onDropPayload: (paneId: string, zone: DropZone, payload: DragPayload) => void;
  setStatus: React.Dispatch<React.SetStateAction<Status>>;
}) {
  if (node.type === "pane") {
    return (
      <TerminalPane
        pane={node}
        active={node.id === activePane}
        token={token}
        onFocus={() => onFocusPane(node.id)}
        onSplit={(direction) => onSplitPane(node.id, direction)}
        onClose={() => onClosePane(node.id)}
        dropTarget={dropTarget?.paneId === node.id ? dropTarget : null}
        onDropTarget={onDropTarget}
        onDropPayload={(zone, payload) => onDropPayload(node.id, zone, payload)}
        setStatus={setStatus}
      />
    );
  }
  return (
    <PanelGroup direction={node.direction} className="h-full w-full min-h-0">
      {node.children.map((child, index) => (
        <React.Fragment key={child.id}>
          {index > 0 ? (
            <PanelResizeHandle
              className={cn(
                "bg-border transition-colors hover:bg-primary",
                node.direction === "horizontal" ? "w-0.5" : "h-0.5",
              )}
            />
          ) : null}
          <Panel minSize={15} className="min-h-0 min-w-0 overflow-hidden">
            <LayoutRenderer
              node={child}
              activePane={activePane}
              token={token}
              onFocusPane={onFocusPane}
              onSplitPane={onSplitPane}
              onClosePane={onClosePane}
              dropTarget={dropTarget}
              onDropTarget={onDropTarget}
              onDropPayload={onDropPayload}
              setStatus={setStatus}
            />
          </Panel>
        </React.Fragment>
      ))}
    </PanelGroup>
  );
}

function TerminalPane({
  pane,
  active,
  token,
  onFocus,
  onSplit,
  onClose,
  dropTarget,
  onDropTarget,
  onDropPayload,
  setStatus,
}: {
  pane: PaneNode;
  active: boolean;
  token: string;
  onFocus: () => void;
  onSplit: (direction: SplitDirection) => void;
  onClose: () => void;
  dropTarget: DropTarget | null;
  onDropTarget: (target: DropTarget | null) => void;
  onDropPayload: (zone: DropZone, payload: DragPayload) => void;
  setStatus: React.Dispatch<React.SetStateAction<Status>>;
}) {
  const terminalRef = React.useRef<HTMLDivElement | null>(null);
  const terminal = React.useRef<Terminal | null>(null);
  const fit = React.useRef<FitAddon | null>(null);
  const socket = React.useRef<WebSocket | null>(null);
  const streamId = React.useRef("");
  const lastSize = React.useRef({ cols: 0, rows: 0 });
  const composing = React.useRef(false);
  const compositionText = React.useRef("");
  const suppressNextText = React.useRef("");
  const activeRef = React.useRef(active);

  React.useEffect(() => {
    activeRef.current = active;
  }, [active]);

  React.useEffect(() => {
    if (!pane.sessionId || !terminalRef.current) return;
    const term = new Terminal({
      cursorBlink: true,
      scrollback: 5000,
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
      fontSize: 14,
      theme: { background: "#050607", foreground: "#eef2f3", cursor: "#35c98f" },
    });
    const fitAddon = new FitAddon();
    term.loadAddon(fitAddon);
    terminalRef.current.innerHTML = "";
    term.open(terminalRef.current);
    terminal.current = term;
    fit.current = fitAddon;
    term.focus();

    streamId.current = makeStreamId(pane.sessionId);
    const ws = new WebSocket(`${wsBaseFromLocation()}/ws/control?token=${encodeURIComponent(token)}`);
    socket.current = ws;

    const fitTerminal = () => {
      if (!terminalRef.current) return;
      const rect = terminalRef.current.getBoundingClientRect();
      if (rect.width < 20 || rect.height < 20) return;
      fitAddon.fit();
      return { cols: term.cols, rows: term.rows };
    };
    const fitAndSendResize = () => {
      const size = fitTerminal();
      if (!size) return;
      if (term.cols === lastSize.current.cols && term.rows === lastSize.current.rows) return;
      lastSize.current = size;
      if (ws.readyState === WebSocket.OPEN) {
        sendEnvelope(ws, "terminal.resize", pane.sessionId!, streamId.current, size);
      }
    };
    const scheduleFit = () => {
      requestAnimationFrame(() => {
        fitAndSendResize();
        requestAnimationFrame(fitAndSendResize);
      });
    };
    scheduleFit();

    ws.addEventListener("open", () => {
      const size = fitTerminal();
      sendEnvelope(ws, "control.open", pane.sessionId!, streamId.current, size || { cols: term.cols, rows: term.rows });
      if (size) lastSize.current = size;
      setStatus({ tone: "ok", title: `Attached ${pane.sessionId}`, detail: streamId.current });
      scheduleFit();
    });
    ws.addEventListener("message", (event) => {
      const env = JSON.parse(event.data);
      if (env.type === "terminal.output") {
        const payload = env.payload || {};
        if (payload.encoding === "base64") term.write(base64ToBytes(payload.data || ""));
        else term.write(payload.data || "");
      }
      if (env.type === "error") {
        setStatus({ tone: "err", title: "Remote error", detail: env.payload?.message || "unknown error" });
      }
    });
    ws.addEventListener("close", () => setStatus({ tone: "warn", title: `Detached ${pane.sessionId}`, detail: "The browser stream closed." }));
    const helperTextarea = terminalRef.current.querySelector<HTMLTextAreaElement>(".xterm-helper-textarea");
    const onCompositionStart = () => {
      composing.current = true;
      compositionText.current = "";
    };
    const onCompositionUpdate = (event: CompositionEvent) => {
      compositionText.current = event.data || "";
    };
    const onCompositionEnd = (event: CompositionEvent) => {
      const data = event.data || compositionText.current;
      composing.current = false;
      compositionText.current = "";
      suppressNextText.current = data;
      if (data) sendEnvelope(ws, "control.input", pane.sessionId!, streamId.current, { data });
      if (helperTextarea) helperTextarea.value = "";
    };
    helperTextarea?.addEventListener("compositionstart", onCompositionStart);
    helperTextarea?.addEventListener("compositionupdate", onCompositionUpdate);
    helperTextarea?.addEventListener("compositionend", onCompositionEnd);
    const terminalElement = terminalRef.current;
    const requestShortcutLock = () => {
      void lockTerminalShortcutKeys();
    };
    const releaseShortcutLock = () => {
      unlockTerminalShortcutKeys();
    };
    terminalElement.addEventListener("pointerdown", requestShortcutLock);
    helperTextarea?.addEventListener("focus", requestShortcutLock);
    helperTextarea?.addEventListener("blur", releaseShortcutLock);
    const dataDisposable = term.onData((data) => {
      if (suppressNextText.current && data === suppressNextText.current) {
        suppressNextText.current = "";
        return;
      }
      if (composing.current && isPrintableText(data)) return;
      suppressNextText.current = "";
      sendEnvelope(ws, "control.input", pane.sessionId!, streamId.current, { data });
    });
    term.attachCustomKeyEventHandler((event) => {
      if (event.type !== "keydown") return true;
      if (!shouldCaptureTerminalKey(event)) return true;
      const data = encodeKeyEvent(event);
      if (!data) return true;
      event.preventDefault();
      event.stopPropagation();
      sendEnvelope(ws, "control.input", pane.sessionId!, streamId.current, { data });
      return false;
    });
    const onDocumentKeyDown = (event: KeyboardEvent) => {
      if (!activeRef.current || event.defaultPrevented) return;
      if (!terminalHasKeyboardFocus(event, terminalRef.current)) return;
      if (!shouldCaptureTerminalKey(event)) return;
      const data = encodeKeyEvent(event);
      if (!data) return;
      event.preventDefault();
      event.stopPropagation();
      event.stopImmediatePropagation();
      term.focus();
      sendEnvelope(ws, "control.input", pane.sessionId!, streamId.current, { data });
    };
    document.addEventListener("keydown", onDocumentKeyDown, true);

    const observer = new ResizeObserver(() => {
      scheduleFit();
    });
    observer.observe(terminalRef.current);

    return () => {
      dataDisposable.dispose();
      document.removeEventListener("keydown", onDocumentKeyDown, true);
      helperTextarea?.removeEventListener("compositionstart", onCompositionStart);
      helperTextarea?.removeEventListener("compositionupdate", onCompositionUpdate);
      helperTextarea?.removeEventListener("compositionend", onCompositionEnd);
      terminalElement.removeEventListener("pointerdown", requestShortcutLock);
      helperTextarea?.removeEventListener("focus", requestShortcutLock);
      helperTextarea?.removeEventListener("blur", releaseShortcutLock);
      unlockTerminalShortcutKeys();
      observer.disconnect();
      if (ws.readyState === WebSocket.OPEN) sendEnvelope(ws, "terminal.close", pane.sessionId!, streamId.current, {});
      ws.close();
      term.dispose();
      terminal.current = null;
      fit.current = null;
      socket.current = null;
    };
  }, [pane.sessionId, token, setStatus]);

  return (
    <div
      className={cn("relative flex h-full min-h-0 min-w-0 flex-col overflow-hidden border-l border-transparent bg-[#050607]", active && "border-l-primary")}
      onMouseDown={onFocus}
      onDragOver={(event) => {
        if (!hasAgentMuxDragPayload(event)) return;
        event.preventDefault();
        event.dataTransfer.dropEffect = "move";
        onDropTarget({ paneId: pane.id, zone: dropZoneFromEvent(event) });
      }}
      onDragLeave={(event) => {
        if (event.currentTarget.contains(event.relatedTarget as Node | null)) return;
        onDropTarget(null);
      }}
      onDrop={(event) => {
        const payload = readDragPayload(event);
        if (!payload) return;
        event.preventDefault();
        onDropPayload(dropZoneFromEvent(event), payload);
      }}
    >
      <div className="flex h-7 items-center justify-between border-b border-border bg-card px-1.5">
        <div
          className="min-w-0 flex-1 cursor-grab truncate text-xs font-medium text-muted-foreground active:cursor-grabbing"
          draggable
          onDragStart={(event) => setDragPayload(event, { kind: "pane", paneId: pane.id })}
          onDragEnd={() => onDropTarget(null)}
          title="Drag pane"
        >
          {pane.sessionId || "Empty pane"}
        </div>
        <div className="flex items-center gap-1">
          <Button
            variant="ghost"
            size="icon-sm"
            onClick={(event) => {
              event.stopPropagation();
              onSplit("horizontal");
            }}
            title="Split right"
          >
            <SplitSquareHorizontal className="h-4 w-4" />
          </Button>
          <Button
            variant="ghost"
            size="icon-sm"
            onClick={(event) => {
              event.stopPropagation();
              onSplit("vertical");
            }}
            title="Split down"
          >
            <SplitSquareVertical className="h-4 w-4" />
          </Button>
          <Button
            variant="ghost"
            size="icon-sm"
            onClick={(event) => {
              event.stopPropagation();
              onClose();
            }}
            title="Close pane"
          >
            <X className="h-4 w-4" />
          </Button>
        </div>
      </div>
      <div className="min-h-0 min-w-0 flex-1 overflow-hidden p-1">
        {pane.sessionId ? (
          <div ref={terminalRef} className="h-full w-full min-w-0 overflow-hidden" />
        ) : (
          <Card className="grid h-full place-items-center border-dashed bg-background">
            <div className="text-center text-sm text-muted-foreground">Select a session from the sidebar.</div>
          </Card>
        )}
      </div>
      <DropIndicator zone={dropTarget?.zone} />
    </div>
  );
}

function DropIndicator({ zone }: { zone?: DropZone }) {
  if (!zone) return null;
  return (
    <div className="pointer-events-none absolute inset-0 z-10">
      <div
        className={cn(
          "absolute rounded-sm border border-primary bg-primary/20",
          zone === "center" && "inset-3",
          zone === "left" && "inset-y-2 left-2 w-1/3",
          zone === "right" && "inset-y-2 right-2 w-1/3",
          zone === "top" && "inset-x-2 top-2 h-1/3",
          zone === "bottom" && "inset-x-2 bottom-2 h-1/3",
        )}
      />
    </div>
  );
}

function newPane(sessionId?: string): PaneNode {
  return { type: "pane", id: crypto.randomUUID(), sessionId };
}

function newWorkspaceTab(sessionId?: string, title?: string): WorkspaceTab {
  const pane = newPane(sessionId);
  return {
    id: crypto.randomUUID(),
    title: title || tabTitleForSession(sessionId) || "Workspace",
    layout: pane,
    activePane: pane.id,
  };
}

function titleForTab(currentTitle: string, layout: LayoutNode) {
  const sessionIDs = collectPanes(layout).map((pane) => pane.sessionId).filter((id): id is string => Boolean(id));
  if (sessionIDs.length === 1) return tabTitleForSession(sessionIDs[0]);
  if (sessionIDs.length > 1) return `${sessionIDs.length} sessions`;
  return currentTitle || "Workspace";
}

function tabTitleForSession(sessionId?: string) {
  if (!sessionId) return "Workspace";
  const parts = sessionId.split("/");
  return parts[parts.length - 1] || sessionId;
}

function updatePane(node: LayoutNode, paneId: string, update: (pane: PaneNode) => PaneNode): LayoutNode {
  if (node.type === "pane") {
    return node.id === paneId ? update(node) : node;
  }
  return { ...node, children: node.children.map((child) => updatePane(child, paneId, update)) };
}

function splitPaneNode(node: LayoutNode, paneId: string, direction: SplitDirection, pane: PaneNode): { node: LayoutNode; found: boolean } {
  if (node.type === "pane") {
    if (node.id !== paneId) return { node, found: false };
    return { node: { type: "split", id: crypto.randomUUID(), direction, children: [node, pane] }, found: true };
  }
  let found = false;
  const children = node.children.map((child) => {
    if (found) return child;
    const result = splitPaneNode(child, paneId, direction, pane);
    found = result.found;
    return result.node;
  });
  return { node: { ...node, children }, found };
}

function insertPaneRelative(node: LayoutNode, paneId: string, zone: DropZone, pane: PaneNode): { node: LayoutNode; inserted: boolean } {
  if (zone === "center") {
    return { node: updatePane(node, paneId, () => pane), inserted: true };
  }
  const direction = zone === "left" || zone === "right" ? "horizontal" : "vertical";
  if (node.type === "pane") {
    if (node.id !== paneId) return { node, inserted: false };
    const children = zone === "left" || zone === "top" ? [pane, node] : [node, pane];
    return { node: { type: "split", id: crypto.randomUUID(), direction, children }, inserted: true };
  }
  let inserted = false;
  const children = node.children.map((child) => {
    if (inserted) return child;
    const result = insertPaneRelative(child, paneId, zone, pane);
    inserted = result.inserted;
    return result.node;
  });
  return { node: { ...node, children }, inserted };
}

function removePane(node: LayoutNode, paneId: string): { node?: LayoutNode; removed: boolean } {
  if (node.type === "pane") {
    return node.id === paneId ? { removed: true } : { node, removed: false };
  }
  let removed = false;
  const children = node.children.flatMap((child) => {
    const result = removePane(child, paneId);
    if (result.removed) removed = true;
    return result.node ? [result.node] : [];
  });
  if (!removed) return { node, removed: false };
  if (children.length === 0) return { removed: true };
  if (children.length === 1) return { node: children[0], removed: true };
  return { node: { ...node, children }, removed: true };
}

function extractPane(node: LayoutNode, paneId: string): { node?: LayoutNode; pane?: PaneNode; removed: boolean } {
  if (node.type === "pane") {
    return node.id === paneId ? { pane: node, removed: true } : { node, removed: false };
  }
  let removed = false;
  let pane: PaneNode | undefined;
  const children = node.children.flatMap((child) => {
    const result = extractPane(child, paneId);
    if (result.removed) {
      removed = true;
      pane = result.pane;
    }
    return result.node ? [result.node] : [];
  });
  if (!removed) return { node, removed: false };
  if (children.length === 0) return { pane, removed: true };
  if (children.length === 1) return { node: children[0], pane, removed: true };
  return { node: { ...node, children }, pane, removed: true };
}

function swapPaneSessions(node: LayoutNode, sourcePaneId: string, targetPaneId: string): LayoutNode {
  const source = findPane(node, sourcePaneId);
  const target = findPane(node, targetPaneId);
  if (!source || !target) return node;
  return updatePane(
    updatePane(node, sourcePaneId, (pane) => ({ ...pane, sessionId: target.sessionId })),
    targetPaneId,
    (pane) => ({ ...pane, sessionId: source.sessionId }),
  );
}

function collectPanes(node: LayoutNode): PaneNode[] {
  if (node.type === "pane") return [node];
  return node.children.flatMap(collectPanes);
}

function findPane(node: LayoutNode, paneId: string): PaneNode | undefined {
  if (node.type === "pane") return node.id === paneId ? node : undefined;
  for (const child of node.children) {
    const pane = findPane(child, paneId);
    if (pane) return pane;
  }
  return undefined;
}

function firstPaneId(node: LayoutNode): string {
  return node.type === "pane" ? node.id : firstPaneId(node.children[0]);
}

function clearSessionFromLayout(node: LayoutNode, sessionId: string): LayoutNode {
  if (node.type === "pane") {
    return node.sessionId === sessionId ? { ...node, sessionId: undefined } : node;
  }
  return { ...node, children: node.children.map((child) => clearSessionFromLayout(child, sessionId)) };
}

function setDragPayload(event: React.DragEvent, payload: DragPayload) {
  event.dataTransfer.effectAllowed = payload.kind === "pane" ? "move" : "copyMove";
  event.dataTransfer.setData(dragMime, JSON.stringify(payload));
  event.dataTransfer.setData("text/plain", payload.kind === "session" ? payload.sessionId : payload.paneId);
}

function readDragPayload(event: React.DragEvent): DragPayload | null {
  const value = event.dataTransfer.getData(dragMime);
  if (!value) return null;
  try {
    const payload = JSON.parse(value) as DragPayload;
    if (payload.kind === "session" && payload.sessionId) return payload;
    if (payload.kind === "pane" && payload.paneId) return payload;
  } catch {
    return null;
  }
  return null;
}

function hasAgentMuxDragPayload(event: React.DragEvent) {
  return Array.from(event.dataTransfer.types).includes(dragMime);
}

function dropZoneFromEvent(event: React.DragEvent<HTMLElement>): DropZone {
  const rect = event.currentTarget.getBoundingClientRect();
  const x = (event.clientX - rect.left) / Math.max(rect.width, 1);
  const y = (event.clientY - rect.top) / Math.max(rect.height, 1);
  const edge = 0.28;
  if (x < edge) return "left";
  if (x > 1 - edge) return "right";
  if (y < edge) return "top";
  if (y > 1 - edge) return "bottom";
  return "center";
}

function sendEnvelope(ws: WebSocket, type: string, sessionId: string, streamId: string, payload: unknown) {
  if (ws.readyState !== WebSocket.OPEN) return;
  ws.send(JSON.stringify({ type, session_id: sessionId, stream_id: streamId, payload }));
}

function base64ToBytes(value: string) {
  const text = atob(value);
  const bytes = new Uint8Array(text.length);
  for (let i = 0; i < text.length; i++) bytes[i] = text.charCodeAt(i);
  return bytes;
}

function shouldCaptureTerminalKey(event: KeyboardEvent) {
  if (event.isComposing || event.metaKey) return false;
  return true;
}

function terminalHasKeyboardFocus(event: Event, element: Element | null) {
  if (!element) return false;
  if (event.target instanceof Node && element.contains(event.target)) return true;
  return document.activeElement instanceof Node && element.contains(document.activeElement);
}

const terminalShortcutLockCodes = [
  "Tab",
  "Space",
  "BracketLeft",
  "BracketRight",
  "Backslash",
  "Minus",
  "Digit6",
  ...Array.from({ length: 26 }, (_, index) => `Key${String.fromCharCode(65 + index)}`),
];

type KeyboardLockNavigator = Navigator & {
  keyboard?: {
    lock?: (keyCodes?: string[]) => Promise<void>;
    unlock?: () => void;
  };
};

async function lockTerminalShortcutKeys() {
  const keyboard = (navigator as KeyboardLockNavigator).keyboard;
  if (!keyboard?.lock) return;
  try {
    await keyboard.lock(terminalShortcutLockCodes);
  } catch {
    // Browsers may reject keyboard lock outside secure contexts or without activation.
  }
}

function unlockTerminalShortcutKeys() {
  (navigator as KeyboardLockNavigator).keyboard?.unlock?.();
}

function isPrintableText(value: string) {
  return Array.from(value).some((char) => char >= " " && char !== "\x7f");
}

function encodeKeyEvent(event: KeyboardEvent) {
  const key = event.key;
  if (key === "Enter") return "\r";
  if (key === "Tab") return event.shiftKey ? "\x1b[Z" : "\t";
  if (key === "Backspace") return "\x7f";
  if (key === "Escape") return "\x1b";
  if (key === "Delete") return "\x1b[3~";
  if (key === "Insert") return "\x1b[2~";
  if (key === "Home") return "\x1b[H";
  if (key === "End") return "\x1b[F";
  if (key === "PageUp") return "\x1b[5~";
  if (key === "PageDown") return "\x1b[6~";
  if (key === "ArrowUp") return "\x1b[A";
  if (key === "ArrowDown") return "\x1b[B";
  if (key === "ArrowRight") return "\x1b[C";
  if (key === "ArrowLeft") return "\x1b[D";
  if (/^F([1-9]|1[0-2])$/.test(key)) return functionKeySequence(key);
  if (event.ctrlKey && !event.altKey) return controlSequence(key);
  if (event.altKey && key.length === 1) return "\x1b" + key;
  if (!event.ctrlKey && !event.altKey && key.length === 1) return key;
  return "";
}

function controlSequence(key: string) {
  if (key === " ") return "\x00";
  if (key === "[") return "\x1b";
  if (key === "\\") return "\x1c";
  if (key === "]") return "\x1d";
  if (key === "^") return "\x1e";
  if (key === "_" || key === "-") return "\x1f";
  if (key.length === 1) {
    const code = key.toUpperCase().charCodeAt(0);
    if (code >= 65 && code <= 90) return String.fromCharCode(code - 64);
  }
  return "";
}

function functionKeySequence(key: string) {
  return (
    {
      F1: "\x1bOP",
      F2: "\x1bOQ",
      F3: "\x1bOR",
      F4: "\x1bOS",
      F5: "\x1b[15~",
      F6: "\x1b[17~",
      F7: "\x1b[18~",
      F8: "\x1b[19~",
      F9: "\x1b[20~",
      F10: "\x1b[21~",
      F11: "\x1b[23~",
      F12: "\x1b[24~",
    } as Record<string, string>
  )[key] || "";
}

function browserDeviceName() {
  return `web:${navigator.platform || "browser"}`;
}

function errorDetail(text: string) {
  try {
    const payload = JSON.parse(text);
    return payload.error || text;
  } catch {
    return text;
  }
}

function errorDetailFromResponseText(status: number, text: string) {
  const detail = text ? errorDetail(text) : "";
  return detail || `${status}`;
}

function sleep(ms: number) {
  return new Promise((resolve) => window.setTimeout(resolve, ms));
}

function setOptionalStorage(key: string, value?: string) {
  if (value) localStorage.setItem(key, value);
  else localStorage.removeItem(key);
}

function shouldRefreshBrowserToken(expiresAt?: string) {
  if (!expiresAt) return true;
  const deadline = Date.parse(expiresAt);
  if (!Number.isFinite(deadline)) return true;
  return deadline - Date.now() <= 2 * 60 * 1000;
}

function isPast(expiresAt?: string) {
  if (!expiresAt) return false;
  const deadline = Date.parse(expiresAt);
  if (!Number.isFinite(deadline)) return false;
  return Date.now() > deadline;
}

function readStoredUser(): AuthUser | null {
  const value = localStorage.getItem("agentmux.user");
  if (!value) return null;
  try {
    const user = JSON.parse(value) as AuthUser;
    if (!user?.email) return null;
    return user;
  } catch {
    return null;
  }
}

function workerDisplayLabel(worker: WorkerView) {
  return worker.name && worker.name !== worker.id ? `${worker.name} (${worker.id})` : worker.id;
}

function workerIsOnline(worker: WorkerView) {
  return worker.online === true || !worker.status || worker.status === "online";
}

function workerEnabled(worker: WorkerView) {
  return worker.enabled !== false;
}

function workerCanStartSession(worker: WorkerView) {
  return workerIsOnline(worker) && workerEnabled(worker);
}

function workerStatusLabel(worker: WorkerView) {
  return workerIsOnline(worker) ? "online" : "offline";
}

function filterWorkers(workers: WorkerView[], query: string) {
  const needle = query.trim().toLowerCase();
  if (!needle) return workers;
  return workers.filter((worker) => {
    const haystacks = [worker.id, worker.name, worker.addr, worker.backend, workerDisplayLabel(worker)];
    return haystacks.some((value) => (value || "").toLowerCase().includes(needle));
  });
}

function filterSessions(sessions: SessionView[], workers: WorkerView[], workerFilter: string, query: string) {
  const workerByID = new Map(workers.map((worker) => [worker.id, worker]));
  const needle = query.trim().toLowerCase();
  return sessions
    .filter((session) => workerFilter === "all" || session.worker_id === workerFilter)
    .filter((session) => {
      if (!needle) return true;
      const worker = workerByID.get(session.worker_id);
      const haystacks = [
        session.id,
        session.name,
        session.worker_id,
        session.cwd,
        session.command,
        session.status,
        session.backend,
        worker?.name,
        worker?.addr,
        worker?.backend,
        worker ? workerDisplayLabel(worker) : "",
      ];
      return haystacks.some((value) => (value || "").toLowerCase().includes(needle));
    })
    .sort((left, right) => left.id.localeCompare(right.id));
}

function sessionBackendLabel(session: SessionView, worker?: WorkerView | null) {
  return session.backend || worker?.backend || "unknown";
}

function stripAnsi(value: string) {
  return value
    .replace(/\x1B\[[0-?]*[ -/]*[@-~]/g, "")
    .replace(/\x1B[PX^_].*?\x1B\\/gs, "")
    .replace(/\x1B[@-_]/g, "")
    .replace(/\r\n/g, "\n")
    .replace(/\r/g, "\n");
}

function formatRelativeTime(value: string) {
  const timestamp = Date.parse(value);
  if (!Number.isFinite(timestamp)) return "unknown";
  const diff = Date.now() - timestamp;
  if (diff < 5_000) return "just now";
  if (diff < 60_000) return `${Math.round(diff / 1000)}s ago`;
  if (diff < 60 * 60_000) return `${Math.round(diff / 60_000)}m ago`;
  if (diff < 24 * 60 * 60_000) return `${Math.round(diff / (60 * 60_000))}h ago`;
  return new Date(timestamp).toLocaleString();
}

function buildWorkerSessionGroups(workers: WorkerView[], sessions: SessionView[], workerFilter: string): WorkerSessionGroup[] {
  const sessionMap = new Map<string, SessionView[]>();
  for (const session of sessions) {
    if (workerFilter !== "all" && session.worker_id !== workerFilter) continue;
    const bucket = sessionMap.get(session.worker_id) || [];
    bucket.push(session);
    sessionMap.set(session.worker_id, bucket);
  }

  const groups: WorkerSessionGroup[] = [];
  for (const worker of workers) {
    if (workerFilter !== "all" && worker.id !== workerFilter) continue;
    const workerSessions = (sessionMap.get(worker.id) || []).slice().sort((left, right) => (left.name || left.id).localeCompare(right.name || right.id));
    if (workerSessions.length === 0) continue;
    groups.push({ worker, workerId: worker.id, sessions: workerSessions });
    sessionMap.delete(worker.id);
  }

  for (const [workerId, workerSessions] of Array.from(sessionMap.entries()).sort(([left], [right]) => left.localeCompare(right))) {
    groups.push({
      worker: null,
      workerId,
      sessions: workerSessions.slice().sort((left, right) => (left.name || left.id).localeCompare(right.name || right.id)),
    });
  }

  return groups;
}

ReactDOM.createRoot(document.getElementById("root")!).render(
  <App />,
);

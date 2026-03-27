import { createBrowserRouter, Navigate } from "react-router";
import { useAuth } from "@/lib/auth";
import Layout from "@/components/Layout";
import Login from "@/pages/Login";
import { lazy, Suspense, type ReactNode } from "react";

const Dashboard = lazy(() => import("@/pages/Dashboard"));
const Decisions = lazy(() => import("@/pages/Decisions"));
const DecisionDetail = lazy(() => import("@/pages/DecisionDetail"));
const Agents = lazy(() => import("@/pages/Agents"));
const Conflicts = lazy(() => import("@/pages/Conflicts"));
const ConflictDetailPage = lazy(() => import("@/pages/ConflictDetail"));
const SearchPage = lazy(() => import("@/pages/SearchPage"));
const ExportPage = lazy(() => import("@/pages/ExportPage"));
const SessionTimeline = lazy(() => import("@/pages/SessionTimeline"));
const GrantsPage = lazy(() => import("@/pages/GrantsPage"));

function PageFallback() {
  return (
    <div className="flex items-center justify-center h-64">
      <div className="animate-pulse text-muted-foreground text-sm">Loading…</div>
    </div>
  );
}

function Lazy({ children }: { children: ReactNode }) {
  return <Suspense fallback={<PageFallback />}>{children}</Suspense>;
}

function AuthGuard({ children }: { children: ReactNode }) {
  const { isAuthenticated } = useAuth();
  if (!isAuthenticated) {
    return <Navigate to="/login" replace />;
  }
  return <>{children}</>;
}

function GuestOnly({ children }: { children: ReactNode }) {
  const { isAuthenticated } = useAuth();
  if (isAuthenticated) {
    return <Navigate to="/" replace />;
  }
  return <>{children}</>;
}

export const router = createBrowserRouter([
  {
    path: "/login",
    element: (
      <GuestOnly>
        <Login />
      </GuestOnly>
    ),
  },
  {
    path: "/",
    element: (
      <AuthGuard>
        <Layout />
      </AuthGuard>
    ),
    children: [
      { index: true, element: <Lazy><Dashboard /></Lazy> },
      { path: "decisions", element: <Lazy><Decisions /></Lazy> },
      { path: "decisions/:runId", element: <Lazy><DecisionDetail /></Lazy> },
      { path: "agents", element: <Lazy><Agents /></Lazy> },
      { path: "conflicts", element: <Lazy><Conflicts /></Lazy> },
      { path: "conflicts/:conflictId", element: <Lazy><ConflictDetailPage /></Lazy> },
      { path: "search", element: <Lazy><SearchPage /></Lazy> },
      { path: "export", element: <Lazy><ExportPage /></Lazy> },
      { path: "grants", element: <Lazy><GrantsPage /></Lazy> },
      { path: "sessions/:sessionId", element: <Lazy><SessionTimeline /></Lazy> },
    ],
  },
]);

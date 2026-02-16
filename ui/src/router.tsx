import { createBrowserRouter, Navigate } from "react-router";
import { useAuth } from "@/lib/auth";
import Layout from "@/components/Layout";
import Login from "@/pages/Login";
import Dashboard from "@/pages/Dashboard";
import Decisions from "@/pages/Decisions";
import DecisionDetail from "@/pages/DecisionDetail";
import Agents from "@/pages/Agents";
import Conflicts from "@/pages/Conflicts";
import SearchPage from "@/pages/SearchPage";
import ExportPage from "@/pages/ExportPage";
import SessionTimeline from "@/pages/SessionTimeline";
import GrantsPage from "@/pages/GrantsPage";
import { type ReactNode } from "react";

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
    element: (
      <AuthGuard>
        <Layout />
      </AuthGuard>
    ),
    children: [
      { index: true, element: <Dashboard /> },
      { path: "decisions", element: <Decisions /> },
      { path: "decisions/:runId", element: <DecisionDetail /> },
      { path: "agents", element: <Agents /> },
      { path: "conflicts", element: <Conflicts /> },
      { path: "search", element: <SearchPage /> },
      { path: "export", element: <ExportPage /> },
      { path: "grants", element: <GrantsPage /> },
      { path: "sessions/:sessionId", element: <SessionTimeline /> },
    ],
  },
]);

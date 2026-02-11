import { createBrowserRouter, Navigate } from "react-router";
import { useAuth } from "@/lib/auth";
import { useConfig } from "@/lib/config";
import Layout from "@/components/Layout";
import Login from "@/pages/Login";
import Dashboard from "@/pages/Dashboard";
import Decisions from "@/pages/Decisions";
import DecisionDetail from "@/pages/DecisionDetail";
import Agents from "@/pages/Agents";
import Conflicts from "@/pages/Conflicts";
import Billing from "@/pages/Billing";
import SearchPage from "@/pages/SearchPage";
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

function BillingGuard({ children }: { children: ReactNode }) {
  const config = useConfig();
  if (!config.billing_enabled) {
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
      {
        path: "billing",
        element: (
          <BillingGuard>
            <Billing />
          </BillingGuard>
        ),
      },
      { path: "search", element: <SearchPage /> },
    ],
  },
]);

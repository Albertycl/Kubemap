import { useCallback, useEffect, useState } from "react";
import { AlertBanner } from "./AlertBanner";
import { api, type Me } from "./api";
import { Login, ChangePassword } from "./Login";
import { SecurityPage } from "./SecurityPage";
import { ClusterList } from "./ClusterList";
import { ClusterPage } from "./ClusterPage";
import { NodePage } from "./NodePage";
import { AppMapPage } from "./AppMapPage";
import { PodsPage } from "./PodsPage";
import { LogsPage } from "./LogsPage";
import { LogSearchPage } from "./LogSearchPage";
import { ProblemsPage } from "./ProblemsPage";
import { DebugPage } from "./DebugPage";
import { ChainPage } from "./ChainPage";
import { NetpolPage } from "./NetpolPage";
import { ServersPage } from "./ServersPage";
import { AccessPage } from "./AccessPage";
import { ArgoPage } from "./ArgoPage";
import { YamlPage } from "./YamlPage";
import { ScenarioList, ScenarioPage } from "./ScenariosPage";
import { AuditPage, InterviewView, JournalEdit, JournalList, JournalView } from "./JournalPage";
import { journalRoute } from "./journal";

/**
 * Routes are in the URL hash: #/ (clusters), #/clusters/<id>, its nodes
 * (#/clusters/<id>/nodes/<node>) and its application map
 * (#/clusters/<id>/apps, one app: #/clusters/<id>/apps/<app id>).
 * The incident journal is #/journal[/<id>[/edit|/interview]] and #/audit
 * (routes in journal.ts).
 */
function useHashRoute(): string {
  const [hash, setHash] = useState(() => window.location.hash || "#/");
  useEffect(() => {
    const on = () => setHash(window.location.hash || "#/");
    window.addEventListener("hashchange", on);
    return () => window.removeEventListener("hashchange", on);
  }, []);
  return hash;
}

/** Decodes a route segment; a malformed one (a hand-typed "%") stays as it is. */
function decode(s: string): string {
  try {
    return decodeURIComponent(s);
  } catch {
    return s;
  }
}

export function App() {
  const [me, setMe] = useState<Me | null | undefined>(undefined); // undefined = loading
  const route = useHashRoute();

  const refreshMe = useCallback(() => {
    api
      .me()
      .then(setMe)
      .catch(() => setMe(null));
  }, []);
  useEffect(refreshMe, [refreshMe]);

  // Any API call that finds the session gone sends the user back to login.
  useEffect(() => {
    const on = () => setMe(null);
    window.addEventListener("kubemap:unauthorized", on);
    return () => window.removeEventListener("kubemap:unauthorized", on);
  }, []);

  if (me === undefined) return <div className="center muted">Loading…</div>;
  if (me === null) return <Login onLogin={setMe} />;
  if (me.mustChangePassword) return <ChangePassword me={me} onDone={setMe} />;

  const sl = route === "#/scenarios";
  const sp = route.match(/^#\/scenarios\/([^/?]+)(?:\?(.*))?$/);
  const m = route.match(/^#\/clusters\/([^/]+)$/);
  const nm = route.match(/^#\/clusters\/([^/]+)\/nodes\/([^/]+)$/);
  const am = route.match(/^#\/clusters\/([^/]+)\/apps(?:\/([^/]+))?$/);
  const pm = route.match(/^#\/clusters\/([^/]+)\/pods(?:\/([^/]+))?$/);
  const lm = route.match(/^#\/clusters\/([^/]+)\/namespaces\/([^/]+)\/pods\/([^/]+)\/logs$/);
  const pr = route.match(/^#\/(?:clusters\/([^/]+)\/)?problems$/);
  const sv = route === "#/servers";
  const np = route.match(/^#\/clusters\/([^/]+)\/netpol$/);
  const dp = route.match(/^#\/clusters\/([^/]+)\/namespaces\/([^/]+)\/pods\/([^/]+)\/debug$/);
  const da = route.match(/^#\/clusters\/([^/]+)\/debug(?:\/app\/([^/]+))?$/);
  const ch = route.match(/^#\/clusters\/([^/]+)\/chain(?:\/([^/?]+))?$/);
  const ls = route.match(/^#\/clusters\/([^/]+)\/log-search(?:\/([^/?]+))?(?:\?(.*))?$/);
  const rb = route.match(/^#\/clusters\/([^/]+)\/rbac(?:\/(who)|\/ns\/([^/?]+))?(?:\?account=([^&]+))?$/);
  const ar = route.match(/^#\/clusters\/([^/]+)\/argocd$/);
  const ym = route.match(/^#\/clusters\/([^/?]+)\/yaml(?:\?(.*))?$/);
  const jr = journalRoute(route);
  const sec = route === "#/security";
  return (
    <div className="app">
      <header className="topbar">
        <a href="#/" className="brand">
          <span className="logo" aria-hidden="true">
            ◈
          </span>
          Kubemap
        </a>
        <nav>
          <a href="#/" className={(pr && !pr[1]) || sv || sl || sp || jr || sec ? "" : "active"}>
            Clusters
          </a>
          <a href="#/servers" className={sv ? "active" : ""}>
            Servers
          </a>
          <a href="#/problems" className={pr && !pr[1] ? "active" : ""}>
            Problems
          </a>
          <a href="#/scenarios" className={sl || sp ? "active" : ""}>
            Scenarios
          </a>
          <a href="#/journal" className={jr && jr.page !== "audit" ? "active" : ""}>
            Journal
          </a>
        </nav>
        <div className="spacer" />
        <a href="#/security" className={route === "#/security" ? "active" : ""} title="Account and two-factor login">
          {me.username}
        </a>
        <button
          className="link"
          onClick={() =>
            api
              .logout()
              .catch(() => {})
              .finally(() => setMe(null))
          }
        >
          Log out
        </button>
      </header>
      <AlertBanner />
      <main>
        {route === "#/security" ? (
          <SecurityPage onChanged={refreshMe} />
        ) : rb ? (
          <AccessPage
            key={route}
            clusterId={decode(rb[1])}
            tab={rb[2] ? "who" : rb[3] ? "namespace" : "account"}
            namespace={rb[3] ? decode(rb[3]) : undefined}
            account={rb[4] ? decode(rb[4]) : undefined}
          />
        ) : ar ? (
          <ArgoPage key={route} clusterId={decode(ar[1])} />
        ) : ym ? (
          <YamlPage key={route} clusterId={decode(ym[1])} query={ym[2]} />
        ) : jr ? (
          jr.page === "list" ? (
            <JournalList />
          ) : jr.page === "new" ? (
            <JournalEdit key={route} kind={jr.kind} />
          ) : jr.page === "edit" ? (
            <JournalEdit key={route} id={jr.id} />
          ) : jr.page === "interview" ? (
            <InterviewView key={route} id={jr.id} />
          ) : jr.page === "audit" ? (
            <AuditPage key={route} cluster={jr.cluster} />
          ) : (
            <JournalView key={route} id={jr.id} />
          )
        ) : sv ? (
          <ServersPage />
        ) : np ? (
          <NetpolPage key={route} clusterId={decode(np[1])} />
        ) : dp ? (
          <DebugPage key={route} clusterId={decode(dp[1])} namespace={decode(dp[2])} pod={decode(dp[3])} />
        ) : ch ? (
          <ChainPage key={route} clusterId={decode(ch[1])} objectId={ch[2] ? decode(ch[2]) : undefined} />
        ) : da ? (
          <DebugPage key={route} clusterId={decode(da[1])} app={da[2] ? decode(da[2]) : undefined} />
        ) : ls ? (
          <LogSearchPage key={`${ls[1]}/${ls[2] ?? ""}`} clusterId={decode(ls[1])} app={ls[2] ? decode(ls[2]) : undefined} query={ls[3]} />
        ) : pr ? (
          <ProblemsPage key={route} clusterId={pr[1] ? decode(pr[1]) : undefined} />
        ) : sp ? (
          <ScenarioPage key={route} id={decode(sp[1])} query={sp[2]} />
        ) : sl ? (
          <ScenarioList />
        ) : lm ? (
          <LogsPage
            key={route}
            clusterId={decode(lm[1])}
            namespace={decode(lm[2])}
            pod={decode(lm[3])}
          />
        ) : pm ? (
          <PodsPage key={route} clusterId={decode(pm[1])} namespace={pm[2] ? decode(pm[2]) : undefined} />
        ) : am ? (
          <AppMapPage
            key={route}
            clusterId={decode(am[1])}
            focusApp={am[2] ? decode(am[2]) : undefined}
          />
        ) : nm ? (
          <NodePage clusterId={decode(nm[1])} node={decode(nm[2])} />
        ) : m ? (
          <ClusterPage id={decode(m[1])} />
        ) : (
          <ClusterList />
        )}
      </main>
    </div>
  );
}

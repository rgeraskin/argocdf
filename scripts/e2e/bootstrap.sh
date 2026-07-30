#!/usr/bin/env bash
# Bootstrap the e2e cluster. Two modes sharing one flow; the BASELINE is the
# realistic one, because a baseline that cannot reproduce production behavior
# only fails later and more confusingly.
#
#   bootstrap.sh           real ArgoCD install, controller included (default).
#                          The controller renders and syncs the children itself,
#                          from the REMOTE repo at targetRevision HEAD - so
#                          master must be PUSHED before this runs (see README
#                          "Running"). Blocks until every Application exists and
#                          is Synced+Healthy, so the suite never sees a half-built
#                          app set; E2E_SYNC_TIMEOUT bounds the wait (900s).
#                          REFUSES a cluster that is already alive: rebuild it
#                          deliberately (mise run e2e:clean) rather than
#                          reinstalling over a running controller.
#   bootstrap.sh --static  no controller: the pinned upstream Application CRD,
#                          control-plane stubs, and every Application a synced
#                          controller would leave behind, applied once by
#                          rendering the charts. Nothing reconciles afterwards,
#                          so it needs no push and no sync wait - the fast loop
#                          for developing fixtures and regenerating expectations.
#
# Both modes are verified to produce IDENTICAL argocdf output: the Application
# set matches by construction, and argocdf reads only .spec and metadata, never
# the .status a controller writes. Expectations are therefore mode-agnostic -
# regenerate on --static, verify on the controller-backed default.
#
# Env (single authority: the argocdf repo's .mise.toml [env]):
#   E2E_KIND_NODE_IMAGE   pinned kind node image (k8s version + digest)
#   E2E_ARGOCD_VERSION    ArgoCD version: the default installs all of it,
#                          --static applies just its Application CRD from
#                          upstream (keep = go.mod argo-cd version)
set -euo pipefail
# Harness script (argocdf repo) operating on the e2e submodule's content.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR/../../e2e"

CTX=kind-argocdf
# The kind node image (k8s version + digest) is part of the expected-output
# contract: argocdf AUTO-DETECTS kube-version and API versions in e2e
# (matching production usage). Bumping the pin => regenerate all expectations
# and review the diff as a k8s behavior change.
NODE_IMAGE="${E2E_KIND_NODE_IMAGE:?set E2E_KIND_NODE_IMAGE (see argocdf .mise.toml [env]) }"

ARGOCD_VERSION="${E2E_ARGOCD_VERSION:?set E2E_ARGOCD_VERSION (see argocdf .mise.toml [env]) }"

STATIC=false
[ "${1:-}" = "--static" ] && STATIC=true

# How long to wait for the controller to converge (controller-backed mode only).
# Generous: the repo-server has to come up, clone the remote, render every child,
# and the depth-2 apps only appear after their parent has synced.
SYNC_TIMEOUT="${E2E_SYNC_TIMEOUT:-900}"

# The baseline has TWO sources of truth for the app set and they must agree: the
# controller builds it from the REMOTE repo at targetRevision HEAD, while
# expected_apps derives it from this working tree. When master is not published
# yet, the wait below cannot succeed - and it burns the whole SYNC_TIMEOUT before
# reporting the app the controller "never created", which describes the symptom
# and hides the cause. Check it before anything is built.
#
# Advisory when the remote cannot be reached: an offline run should not be blocked
# by a check it cannot perform, and the convergence wait is still the backstop.
if [ "$STATIC" = false ]; then
  local_head="$(git rev-parse HEAD)"
  remote_head="$(git ls-remote origin master 2>/dev/null | awk '{print $1}' | head -1)"
  dirty="$(git status --porcelain)"

  if [ -z "$remote_head" ]; then
    echo "WARNING: could not read origin/master (offline?); skipping the published-master check" >&2
  elif [ "$local_head" != "$remote_head" ] || [ -n "$dirty" ]; then
    echo "ERROR: this e2e working tree is not what the remote serves, so the controller cannot" >&2
    echo "       build the app set the suite expects" >&2
    echo "         working tree:  ${local_head:0:12}$([ -n "$dirty" ] && echo ' + uncommitted changes')" >&2
    echo "         origin/master: ${remote_head:0:12}" >&2
    echo "       the baseline controller syncs the REMOTE repo at targetRevision HEAD, so an" >&2
    echo "       unpublished fixture or case never appears and bootstrap waits ${SYNC_TIMEOUT}s for it" >&2
    echo "       publish first:                  mise run e2e:push" >&2
    echo "       or author without a controller: mise run e2e:bootstrap-static" >&2
    exit 1
  fi
fi

# expected_apps lists the Applications the cluster must end up holding, derived
# from the SAME renders the static path applies - so both modes are held to one
# definition of the app set instead of each having its own idea.
expected_apps() {
  echo root-app
  {
    helm template root-app charts/apps --values charts/apps/values-apps.yaml
    helm template nested-apps apps/nested-apps
  } | yq -r 'select(.kind == "Application") | .metadata.name' | grep -vE '^(---|null)$'
}

# wait_for_controller blocks until the controller has finished building the app
# set and every Application is Synced+Healthy.
#
# Without this, bootstrap returns while the controller is still working and the
# suite sees an INCOMPLETE app set: cases fail on affected-app counts, which reads
# as an argocdf regression rather than a cluster that was not ready. The depth-2
# apps make it worse - the grandchild only exists after its parent has synced, so
# "some Applications exist" is not a usable signal.
#
# Waiting on names rather than a count catches a wrong app as well as a missing
# one, and `kubectl wait --all` is not enough on its own: it only covers the
# Applications that exist at the moment it is called, so it would happily pass
# before the grandchild was created.
wait_for_controller() {
  echo "waiting for ArgoCD to come up..."
  kubectl --context "$CTX" -n argocd rollout status statefulset/argocd-application-controller --timeout=300s
  kubectl --context "$CTX" -n argocd rollout status deployment/argocd-repo-server --timeout=300s

  local expected total present missing notready deadline
  expected="$(expected_apps | sort -u)"
  total=$(printf '%s\n' "$expected" | wc -l | tr -d ' ')
  echo "waiting for $total Applications to be created and reach Synced+Healthy (timeout ${SYNC_TIMEOUT}s)..."

  deadline=$((SECONDS + SYNC_TIMEOUT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    present="$(kubectl --context "$CTX" -n argocd get applications.argoproj.io \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | sort -u)"
    missing="$(comm -23 <(printf '%s\n' "$expected") <(printf '%s\n' "$present"))"
    notready="$(kubectl --context "$CTX" -n argocd get applications.argoproj.io \
      -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.sync.status}{" "}{.status.health.status}{"\n"}{end}' 2>/dev/null \
      | awk 'NF && ($2 != "Synced" || $3 != "Healthy")')"
    if [ -z "$missing" ] && [ -z "$notready" ]; then
      echo "all $total Applications Synced+Healthy"
      return 0
    fi
    sleep 5
  done

  # A bare timeout says nothing about which half failed, so name both.
  echo "ERROR: the controller did not converge within ${SYNC_TIMEOUT}s" >&2
  [ -z "$missing" ] || { echo "  never created:" >&2; printf '    %s\n' $missing >&2; }
  [ -z "$notready" ] || { echo "  not Synced+Healthy (name sync health):" >&2; printf '    %s\n' "$notready" >&2; }
  echo "       raise the budget with E2E_SYNC_TIMEOUT, or inspect: kubectl --context $CTX -n argocd get applications" >&2
  exit 1
}

# controller_present reports whether the cluster holds a REAL ArgoCD control plane,
# i.e. was built by the baseline. It is what tells the two cluster modes apart
# after the fact: the Application sets are identical by construction, so the
# controller is the only difference that matters.
controller_present() {
  kubectl --context "$CTX" -n argocd get statefulset argocd-application-controller >/dev/null 2>&1
}

if ! kind get clusters 2>/dev/null | grep -qx argocdf; then
  # The config carries the host port mapping for the private registry.
  kind create cluster -n argocdf --image "$NODE_IMAGE" --config "$SCRIPT_DIR/kind-config.yaml"
else
  # NEITHER mode reuses a cluster built by the other, and both refusals are about
  # the controller.
  #
  # Baseline over anything alive: re-installing ArgoCD reapplies the control plane
  # while a controller reconciles, and what goes wrong (a half-applied upgrade, a
  # repo-server still serving old manifests) surfaces later as case failures that
  # blame the fixtures.
  #
  # --static over a BASELINE cluster: root-app is syncPolicy.automated, so the
  # controller OWNS the child Applications and renders them from the REMOTE repo at
  # targetRevision HEAD. Applying the local app set on top is drift. It survives at
  # first only because `automated: {}` sets neither selfHeal nor prune - and it is
  # reverted the moment the remote revision changes, which is exactly what
  # e2e:push does. So the expectations would be regenerated against one app set and
  # verified against another, with nothing announcing the switch. --static is
  # idempotent on a --static cluster, where nothing reconciles behind it; that is
  # the property the authoring loop relies on, and it does not extend to a cluster
  # with a live controller.
  if [ "$STATIC" = false ]; then
    echo "ERROR: kind cluster 'argocdf' is already running" >&2
    if controller_present; then
      echo "       it already runs the baseline control plane - rebuild it deliberately:" >&2
      echo "         mise run e2e:clean && mise run e2e:bootstrap" >&2
    else
      echo "       it is a --static cluster; re-apply the app set in place with" >&2
      echo "         mise run e2e:bootstrap-static" >&2
      echo "       or rebuild the baseline: mise run e2e:clean && mise run e2e:bootstrap" >&2
    fi
    exit 1
  fi
  if controller_present; then
    echo "ERROR: kind cluster 'argocdf' runs the REAL ArgoCD controller (baseline mode)" >&2
    echo "       --static would apply the app set under a controller that owns root-app and" >&2
    echo "       syncs it from the REMOTE repo, so the children revert as soon as the remote" >&2
    echo "       revision changes (e2e:push) - silently, mid-run" >&2
    echo "       for the fast authoring loop, rebuild without the controller:" >&2
    echo "         mise run e2e:clean && mise run e2e:bootstrap-static" >&2
    exit 1
  fi
  # An existing cluster on a different node image would silently render
  # against the wrong API surface (the pin is part of the expected-output
  # contract) - fail loudly instead of reusing it. Best-effort: skipped when
  # the node container can't be inspected (non-docker provider).
  current="$(docker inspect -f '{{.Config.Image}}' argocdf-control-plane 2>/dev/null || true)"
  if [ -n "$current" ] && [ "$current" != "$NODE_IMAGE" ]; then
    echo "ERROR: existing kind cluster 'argocdf' runs node image" >&2
    echo "         $current" >&2
    echo "       but E2E_KIND_NODE_IMAGE pins" >&2
    echo "         $NODE_IMAGE" >&2
    echo "       recreate it: kind delete cluster -n argocdf && mise run e2e:bootstrap" >&2
    exit 1
  fi
  # Same loud failure for a cluster created before the registry port mapping
  # existed (kind-config.yaml) - the auth cases would fail confusingly.
  if [ -n "$current" ] && ! docker port argocdf-control-plane 2>/dev/null | grep -q 30517; then
    echo "ERROR: existing kind cluster 'argocdf' lacks the registry port mapping (5317->30517)" >&2
    echo "       recreate it: kind delete cluster -n argocdf && mise run e2e:bootstrap" >&2
    exit 1
  fi
fi

kubectl --context "$CTX" apply -f bootstrap/namespace.yaml

# --- private OCI registry (both modes; see bootstrap/registry.yaml) ----------
REGISTRY_HOST="127.0.0.1.nip.io:5317"

# Self-signed TLS for the registry (generated per cluster, never committed;
# the repository Secret sets insecure: "true").
if ! kubectl --context "$CTX" -n argocd get secret registry-tls >/dev/null 2>&1; then
  tlsdir=$(mktemp -d)
  openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
    -keyout "$tlsdir/tls.key" -out "$tlsdir/tls.crt" \
    -subj "/CN=127.0.0.1.nip.io" \
    -addext "subjectAltName=DNS:127.0.0.1.nip.io" >/dev/null 2>&1
  kubectl --context "$CTX" -n argocd create secret tls registry-tls \
    --cert="$tlsdir/tls.crt" --key="$tlsdir/tls.key"
  rm -rf "$tlsdir"
fi
kubectl --context "$CTX" apply -f bootstrap/registry.yaml
kubectl --context "$CTX" apply -f bootstrap/repo-secret.yaml

# CoreDNS: authoritative in-cluster answer for the registry hostname -> the
# Service's fixed ClusterIP, so pods (the repo-server, in the default mode) reach the
# registry through the SAME URL the host uses, without consulting nip.io.
if ! kubectl --context "$CTX" -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}' | grep -q nip.io; then
  corefile=$(mktemp)
  kubectl --context "$CTX" -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}' \
    | awk '/^ *forward /{print "    hosts {"; print "       10.96.100.100 127.0.0.1.nip.io"; print "       fallthrough"; print "    }"} {print}' > "$corefile"
  kubectl --context "$CTX" -n kube-system create configmap coredns \
    --from-file=Corefile="$corefile" --dry-run=client -o yaml \
    | kubectl --context "$CTX" -n kube-system apply -f -
  rm -f "$corefile"
  kubectl --context "$CTX" -n kube-system rollout restart deployment coredns
fi

kubectl --context "$CTX" -n argocd rollout status deployment registry --timeout=120s

# Seed the private chart (0.1.0 = master's catalog pin, 0.2.0 = the version
# the case branch bumps to). Auth via a SCRATCH registry config written
# directly - never `helm registry login` (helm's native-store detection would
# route the login to the OS keychain) and never the user's config.
seed=$(mktemp -d)
printf '{"auths":{"%s":{"auth":"%s"}}}' "$REGISTRY_HOST" "$(printf 'e2e:e2e' | base64)" > "$seed/registry.json"
for i in $(seq 1 30); do
  curl -ksf -u e2e:e2e "https://$REGISTRY_HOST/v2/" >/dev/null && break
  [ "$i" = 30 ] && { echo "registry not reachable at $REGISTRY_HOST" >&2; exit 1; }
  sleep 2
done
for v in 0.1.0 0.2.0; do
  helm package bootstrap/charts/private-app --version "$v" -d "$seed" >/dev/null
  HELM_REGISTRY_CONFIG="$seed/registry.json" helm push "$seed/private-app-$v.tgz" \
    "oci://$REGISTRY_HOST/charts" --insecure-skip-tls-verify >/dev/null 2>&1 \
    || { echo "chart push failed for private-app $v" >&2; exit 1; }
done
rm -rf "$seed"
# ------------------------------------------------------------------------------

if ! $STATIC; then
  kubectl --context "$CTX" apply -n argocd --server-side --force-conflicts \
    -f "https://raw.githubusercontent.com/argoproj/argo-cd/${ARGOCD_VERSION}/manifests/install.yaml"
  # kustomize-helm fixtures use kustomize's helmCharts inflation, which ArgoCD
  # only runs when configured (same option argocdf receives via
  # --kustomize-enable-helm).
  kubectl --context "$CTX" -n argocd patch configmap argocd-cm --type merge \
    -p '{"data":{"kustomize.buildOptions":"--enable-helm"}}'
  # Recent kind node images enforce NetworkPolicies (kindnetd embeds
  # kube-network-policies); under them ArgoCD's stock policies blackhole
  # pod-to-pod traffic AND kubelet probes here (DNS i/o timeouts, liveness
  # crash loops). They protect nothing on a local test cluster - drop them.
  kubectl --context "$CTX" -n argocd delete networkpolicies \
    -l app.kubernetes.io/part-of=argocd
  # HPAs (external-podinfo's kustomize source ships one) stay Degraded forever
  # without metrics; kind kubelets serve self-signed certs, hence the flag.
  # Pinned (not @latest) to match the rest of the e2e pin philosophy, even
  # though only the controller-backed mode installs it.
  kubectl --context "$CTX" apply \
    -f https://github.com/kubernetes-sigs/metrics-server/releases/download/v0.7.2/components.yaml
  kubectl --context "$CTX" -n kube-system patch deployment metrics-server --type json \
    -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
else
  # Only the Application CRD: argocdf just LISTS Application CRs. Sourced from
  # the pinned upstream version instead of a vendored copy.
  kubectl --context "$CTX" apply --server-side --force-conflicts \
    -f "https://raw.githubusercontent.com/argoproj/argo-cd/${ARGOCD_VERSION}/manifests/crds/application-crd.yaml"
  # --repo-creds cluster goes through ArgoCD's settings manager, which expects
  # the control-plane ConfigMap/Secret to exist (the default mode installs the
  # real ones with install.yaml, so these stubs must not be applied there).
  kubectl --context "$CTX" apply -f bootstrap/argocd-stubs.yaml
fi

# apply returns before the CRD is Established; applying an Application CR in
# that window fails with "resource mapping not found"
kubectl --context "$CTX" wait --for=condition=Established --timeout=60s \
  crd/applications.argoproj.io

kubectl --context "$CTX" apply -f bootstrap/root-app.yaml

if $STATIC; then
  # Controller stand-in: materialize the child Application CRs from master's
  # catalog so argocdf can list them.
  helm template root-app charts/apps --values charts/apps/values-apps.yaml \
    | kubectl --context "$CTX" apply -f -
  # ...and the DEPTH-2 Applications that syncing those children would create, so
  # the static Application set matches what a real controller leaves behind
  # (the default mode's steady state, and what production always looks like: a synced
  # apps-of-apps child exists in the cluster AND in its parent's render).
  # Without this, no case exercised depth-2 resolution of an app present in both
  # places, and what is now case/grandchild-spec-change silently depended on the
  # cluster being unable to sync.
  # apps/nested-apps is the ONLY fixture that renders an Application, and it
  # renders nothing else. A new such fixture has to be added here as well; that
  # is deliberate over rendering every child and applying whatever `kind:
  # Application` falls out, which is this script reimplementing the controller's
  # recursion. case/grandchild-add is what proves an app reachable ONLY through
  # a parent's render is still discovered.
  helm template nested-apps apps/nested-apps | kubectl --context "$CTX" apply -f -
else
  wait_for_controller
fi

echo "e2e cluster ready ($($STATIC && echo static || echo controller-backed)): $(kubectl --context "$CTX" get applications.argoproj.io -n argocd --no-headers | wc -l | tr -d ' ') Applications"

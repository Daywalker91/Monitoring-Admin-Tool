# Server in k3s

Manifeste für kustomize: Namespace `monitoring`, Longhorn-Volume für die SQLite-Datenbank,
Deployment (1 Replica, `Recreate`), Service mit fester MetalLB-IP. Der Server macht TLS selbst
(Zertifikat aus `mat-server gen-cert`), damit Agenten es per `-ca` pinnen können.

Image: `ghcr.io/daywalker91/mat-server` (amd64 + arm64), gebaut vom Workflow
`.github/workflows/server-image.yml` bei jedem Push auf `main`.

## Einmalig vorbereiten

```powershell
# 0. IP anpassen: service.yaml → metallb.universe.tf/loadBalancerIPs (freie IP aus einem MetalLB-Pool)

# 1. Namespace
kubectl create namespace monitoring

# 2. Zugriff auf das private Image: GitHub-Token (classic) mit Recht "read:packages"
kubectl -n monitoring create secret docker-registry ghcr-pull `
  --docker-server=ghcr.io --docker-username=Daywalker91 --docker-password=<TOKEN>

# 3. TLS-Zertifikat für die Service-IP (und ggf. einen DNS-Namen)
mat-server gen-cert -hosts 192.168.4.56,monitor.lan -out data
kubectl -n monitoring create secret tls mat-server-tls --cert data\server.crt --key data\server.key

# 4. Tokens und Benachrichtigungen (Werte nie committen; Vorlage: secret.example.yaml)
kubectl -n monitoring create secret generic mat-server-env `
  --from-literal=MAT_AGENT_TOKEN=<langes-zufälliges-token> `
  --from-literal=MAT_NTFY_URL=https://ntfy.sh/<topic>
```

## Anwenden

```powershell
kubectl apply -k deploy/k8s
kubectl -n monitoring rollout status deploy/mat-server

# GUI-Benutzer anlegen (Passwort wird abgefragt)
kubectl -n monitoring exec -it deploy/mat-server -- mat-server user add admin
```

GUI: `https://192.168.4.56/` (Browser-Warnung bis zum Import von `server.crt`).

Agenten verbinden sich mit `wss://192.168.4.56/ws/agent` und pinnen das Zertifikat:

```sh
sudo sh install-agent.sh --server wss://192.168.4.56/ws/agent --ca server.crt
```

## Mit ArgoCD

Statt `kubectl apply -k` eine Application auf dieses Repo zeigen lassen (das Repo ist privat,
ArgoCD braucht dafür Zugangsdaten). Die Secrets aus Schritt 2–4 bleiben außerhalb von Git.

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: mat-server
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/Daywalker91/Monitoring-Admin-Tool.git
    targetRevision: main
    path: deploy/k8s
  destination:
    server: https://kubernetes.default.svc
    namespace: monitoring
  syncPolicy:
    automated: { prune: true, selfHeal: true }
```

Mit dem Tag `main` und `imagePullPolicy: Always` holt ein Neustart des Pods das neueste Image:
`kubectl -n monitoring rollout restart deploy/mat-server`. Für feste Stände einen Git-Tag `v1.2.3`
setzen und in `kustomization.yaml` `newTag: 1.2.3` eintragen.

## Zugang von außen (optional)

Ein Reverse-Proxy im Cluster (z. B. das Caddy-Gateway) kann vor den Server geschaltet werden.
`MAT_TRUSTED_PROXIES` steht auf dem Pod-Netz `10.42.0.0/16`; von dort übernimmt der Server
`X-Forwarded-For` und `X-Forwarded-Proto` (echte Client-IP, `Secure`-Cookie).

Empfehlung: von außen nur die GUI und nur mit zusätzlichem Schutz am Gateway (VPN, IP-Allowlist
oder vorgeschaltete Anmeldung). `/metrics` nie nach außen; `/ws/agent` nur, wenn Agenten außerhalb
des LANs laufen.

## Hinweise

- Agenten laufen **nicht** als Pod, sondern direkt auf den Hosts (auch auf den k3s-Nodes per
  `install-agent.sh`), sonst sähen sie nur ihren Container.
- Backup: die Datenbank liegt auf dem Longhorn-Volume `mat-server-data` (Longhorn-Snapshots/Backups).

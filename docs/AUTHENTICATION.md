# Authentication Guide

The Namespace Resizer Controller supports two methods for authenticating with GitHub:

1.  **GitHub App (Recommended for Production)**
2.  **Personal Access Token (PAT) (Easier for Development)**

## Common Configuration

Regardless of the authentication method, you must provide the following environment variables to identify the target repository and cluster:

| Variable       | Description                                   | Example           |
| :------------- | :-------------------------------------------- | :---------------- |
| `GITHUB_OWNER` | The GitHub organization or user               | `payback159`      |
| `GITHUB_REPO`  | The repository name                           | `ns-resizer-demo` |
| `CLUSTER_NAME` | The name of the cluster (used for file paths) | `prod-cluster`    |

---

## Option 1: GitHub App (Recommended)

Using a GitHub App is more secure because it uses short-lived tokens and granular permissions.

### 1. Create a GitHub App
1.  Go to **Settings > Developer settings > GitHub Apps > New GitHub App**.
2.  **Name:** `Namespace Resizer Controller` (or similar).
3.  **Homepage URL:** Your project URL (e.g., repo URL).
4.  **Webhook:** Active (you can use a dummy URL if not using webhooks yet).
5.  **Permissions:**
    *   **Contents:** `Read & Write` (to read quotas and create branches/commits).
    *   **Pull Requests:** `Read & Write` (to create PRs).
    *   **Metadata:** `Read-only` (mandatory).
6.  **Create App**.

### 2. Install the App
1.  After creation, go to **Install App** in the sidebar.
2.  Install it on the target repository (or organization).
3.  Note the **Installation ID** from the URL (e.g., `.../installations/12345678`).

### 3. Get Credentials
1.  **App ID:** Found in the "About" section of the App settings.
2.  **Private Key:** Generate a private key in the "Private keys" section and download the `.pem` file.

### 4. Configure Controller
Set the following environment variables:

| Variable                 | Description                    |
| :----------------------- | :----------------------------- |
| `GITHUB_APP_ID`          | The App ID (integer)           |
| `GITHUB_INSTALLATION_ID` | The Installation ID (integer)  |
| `GITHUB_PRIVATE_KEY`     | The content of the `.pem` file |

**Kubernetes Secret Example:**
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: github-auth
  namespace: namespace-resizer-system
type: Opaque
stringData:
  GITHUB_APP_ID: "12345"
  GITHUB_INSTALLATION_ID: "987654"
  GITHUB_PRIVATE_KEY: |
    -----BEGIN RSA PRIVATE KEY-----
    ...
    -----END RSA PRIVATE KEY-----
```

---

## Option 2: Personal Access Token (PAT)

Useful for local development or quick testing.

### 1. Generate Token
1.  Go to **Settings > Developer settings > Personal access tokens > Tokens (classic)**.
2.  Generate new token.
3.  **Scopes:** `repo` (Full control of private repositories).

### 2. Configure Controller
Set the following environment variable:

| Variable       | Description                                    |
| :------------- | :--------------------------------------------- |
| `GITHUB_TOKEN` | The Personal Access Token (starts with `ghp_`) |

**Note:** PATs are tied to a specific user and have broad permissions. Use with caution in production.

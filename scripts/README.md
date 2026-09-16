# Scripts

| Script | Purpose |
| --- | --- |
| `generate-crds.py` | Regenerate project CRDs from the bounded API schema |
| `install-gateway-crds.py` | Verify and install the pinned Gateway API CRDs |
| `install.py` | Render or apply namespaced controller RBAC and Deployment |
| `cleanup.py` | Delete only empty, exactly owned, archived NLBs after retirement |
| `collect-notices.py` | Regenerate the dependency notice bundle |
| `package.py` | Build, verify, and safely extract a release archive |
| `audit-public.py` | Detect customer/account residue, likely secrets, public IPs, forbidden directories, and broken local links |

Render and inspect changes before applying them. OCI integration harnesses and account-specific automation are intentionally outside this public repository.

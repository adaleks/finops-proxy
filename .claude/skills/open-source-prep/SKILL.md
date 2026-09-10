# Skill: Open-Source Readiness Checklist

Verification steps prior to publishing the codebase on GitHub:
1. **Zero Secret Leakage:** Verify no hardcoded API keys, bearer tokens, or internal IP addresses exist in source code, tests, or config samples.
2. **Licensing:** Ensure the root repository contains the Apache 2.0 LICENSE file and proper copyright notices.
3. **Local Autonomy:** The core proxy must be fully functional offline or within a local Docker container in under 30 seconds without external SaaS dependencies.

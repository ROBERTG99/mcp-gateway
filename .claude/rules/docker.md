---
paths: ["Dockerfile", "compose*.yaml", ".devcontainer/**"]
---

# Docker rules

- One multi-stage `Dockerfile`; runtime image `gcr.io/distroless/static-debian12:nonroot`; exec-form `HEALTHCHECK`.
- Declare `ARG HTTPS_PROXY` in every stage that downloads; build with host networking in the cloud (Appendix A).
- No `apt-get` in Dockerfiles. Containers make no internet calls at runtime.
- Host ports stay in 8200-8299; every service has a healthcheck and `depends_on: condition: service_healthy`.
- `hadolint` must be clean.

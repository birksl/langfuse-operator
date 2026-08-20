---
layout: home
hero:
  name: Langfuse Operator
  text: Production-Ready Langfuse on Kubernetes
  tagline: A Kubernetes operator for deploying and managing the full Langfuse LLM observability stack.
  actions:
    - theme: brand
      text: Get Started
      link: /guide/what-is-langfuse-operator
    - theme: alt
      text: View on GitHub
      link: https://github.com/PalenaAI/langfuse-operator
  image:
    src: /logo.svg
    alt: Langfuse Operator

features:
  - icon: "\U0001F680"
    title: Full Stack Deployment
    details: Deploy Web, Worker, PostgreSQL, ClickHouse, Redis, and Blob Storage with a single custom resource.
  - icon: "\U0001F504"
    title: Automated Operations
    details: Rolling upgrades, migration Jobs gated on what was actually migrated, pod restarts on secret rotation, and circuit breakers out of the box.
  - icon: "\U0001F465"
    title: Multi-Tenancy
    details: Manage organizations, projects, and API keys declaratively through Kubernetes CRDs.
  - icon: "\U0001F512"
    title: Security First
    details: Read-only root filesystem, non-root execution, NetworkPolicies, and automatic secret generation.
  - icon: "\U0001F4CA"
    title: Observability
    details: Every datastore probed and reported as status conditions, OpenTelemetry export from Langfuse itself, and controller-runtime metrics when enabled.
  - icon: "\U0001F30D"
    title: Platform Agnostic
    details: Works on vanilla Kubernetes, OpenShift, EKS, GKE, and AKS with native integrations.
---

========
ETOS API
========

.. image:: https://img.shields.io/badge/Stage-Sandbox-yellow.svg
  :target: https://github.com/eiffel-community/community/blob/master/PROJECT_LIFECYCLE.md#stage-sandbox

ETOS (Eiffel Test Orchestration System) API


Description
===========

API for starting ETOS executions and for getting extra information about the system.


Installation
============

   pip install .


Running dockers in development mode
===================================

   make DEPLOY=etos-iut start
   make DEPLOY=etos-logarea start
   make DEPLOY=etos-sse start


Building the SSE image
=====================

``make sse-docker`` passes the current Git commit to the Docker build so that
the SSE server logs it even with Go VCS stamping disabled. Git metadata is
not copied into the SSE build container.

The ``version`` log field is the short commit SHA and identifies the exact
source.

For a direct Docker build, supply revision explicitly::

   docker build --build-arg BUILD_REVISION="$(git rev-parse --short HEAD)" \
     -f deploy/etos-sse/Dockerfile -t etos-sse:local .

The SSE image build fails if ``BUILD_REVISION``is missing or empty.


SSE tracing
===========

The ``/sse/v2alpha/events/:identifier`` endpoint emits one server span per
connection attempt when ``OTEL_EXPORTER_OTLP_ENDPOINT`` or
``OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`` is set. Export uses OTLP/gRPC with the
same resource conventions as SSE metrics (default service name ``etos-sse``).
``OTEL_TRACES_EXPORTER=none`` or ``OTEL_SDK_DISABLED=true`` disables SSE tracing.
Trace setup/export failures do not stop the SSE service.
Legacy SSE v1/v1alpha routes and self-test endpoints are not traced.

Incoming W3C ``traceparent`` and ``tracestate`` headers are added as a span
link, not used as the parent; the ``baggage`` header is extracted and
propagated. Each SSE connection therefore starts a separate trace, linked to
the upstream trace when a valid ``traceparent`` is provided.
Spans include ``etos.id``, the route template, reconnect intent, HTTP status
and close reason. Backend/write failures have bounded ``error.type`` values;
event payloads, query strings, credentials and baggage are not exported as 
span attributes. Normal disconnects and server shutdown are not marked as errors.

Connection spans are exported after the handler exits, not while a stream is
still open. Use the existing SSE metrics for live connections.


Contribute
==========

- Issue Tracker: https://github.com/eiffel-community/etos/issues
- Source Code: https://github.com/eiffel-community/etos-api


Support
=======

If you are having issues, please let us know.
There is a mailing list at: etos-maintainers@googlegroups.com or just write an Issue.

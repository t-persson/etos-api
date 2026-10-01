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


Metrics
=======

The Python ETOS API exposes Prometheus metrics at ``GET /metrics``. The metrics below are
alpha: their names, labels, and buckets may change.

Every metric has an ``etos_version`` label. It is the ETOS release from the ``ETOS_VERSION``
environment variable set by the ETOS controller, or ``unknown``.

``etos_api_build_info{etos_version,version}``
   Always ``1``. ``version`` is the ``etos_api`` package version.

``http_requests_total{endpoint,operation,status,etos_version}`` and ``http_request_duration_seconds{endpoint,operation,etos_version}``
   One observation per handled request to the start, abort, and sub-suite endpoints in every
   API version. ``status`` is the response status code. Requests rejected by schema validation
   are not included.

The following metrics are recorded for the start endpoints ``POST /api/v0/etos`` (also served
at ``POST /api/etos``), ``POST /api/v1alpha/testrun``, and ``POST /api/v1beta1/testrun``. The
``api_version`` label is ``v0``, ``v1alpha``, or ``v1beta1``, so the series show how users move
between API versions. They use bounded label values and never contain TestRun, suite, or
artifact identifiers. Every known label combination is exported from start-up, so a zero value
is distinguishable from a missing series.

``etos_api_testrun_submissions_total{api_version,outcome,etos_version}`` and ``etos_api_testrun_submission_duration_seconds{api_version,outcome,etos_version}``
   One observation per submission. ``outcome`` is one of the following:

   * ``success``: the execution was started (200).
   * ``request_invalid``: the request body failed schema validation (422). The duration
     histogram has no series for this outcome, because the request is rejected before it is
     handled.
   * ``suite_download_failed``: the test suite could not be downloaded (400 in ``v0`` and
     ``v1beta1``, 500 in ``v1alpha``).
   * ``suite_invalid``: the test suite, its test runner images, or the datasets are invalid (400).
   * ``artifact_not_found``: the artifact was not found in the event repository in time (400).
   * ``artifact_lookup_timeout``: the event repository query timed out (504).
   * ``event_repository_error``: the event repository query failed (400).
   * ``environment_configuration_failed``: ``v0`` only. The environment provider could not be
     configured (400, or 500 for an unexpected error).
   * ``event_publish_failed``: ``v0`` only. The test execution recipe event could not be
     published (500).
   * ``testrun_create_failed``: ``v1alpha`` and ``v1beta1`` only. The Kubernetes TestRun could
     not be created (500).
   * ``internal_error``: any other unexpected failure (500).

``etos_api_event_repository_queries_total{api_version,query,outcome,etos_version}``
   One increment per query attempt while waiting for the artifact. ``query`` is
   ``artifact_by_id`` or ``artifact_by_identity``. ``outcome`` is ``found``, ``not_ready``
   (the query is retried), or ``error``.

``etos_api_artifact_lookup_duration_seconds{api_version,query,outcome,etos_version}``
   The total time spent waiting for the artifact. ``outcome`` is ``found``, ``not_found``
   (the wait timed out), or ``error``.

The Go SSE service separately exposes Prometheus metrics at ``GET /metrics`` on its service
port (``GET /sse/metrics`` through the existing ``/sse`` ingress path). It does not export
metrics over OTLP. Every SSE metric has the ``etos_version`` label from ``ETOS_VERSION`` set
by the ETOS controller, or ``unknown`` when unset.

Only the ``v2alpha`` event stream is instrumented for v1beta TestRun delivery. The ETOS CLI's
v2alpha SSE client constructs ``/sse/v2alpha/events/{identifier}`` and sends
``Last-Event-ID`` when resuming. The v1alpha endpoint streams legacy logs from ``/log``;
``v1`` streams events from the legacy Log Listener ``/v1/log``. Neither carries the v1beta
validated result-event stream. A v2alpha ``shutdown`` event is the terminal TestRun result and
contains its ``conclusion`` and ``verdict``.

``etos_sse_build_info{etos_version}``
   Always ``1``. The ETOS release version comes from ``ETOS_VERSION``.

``etos_sse_reconnects_total{api_version,outcome,etos_version}``
   Counts requests that include ``Last-Event-ID``. ``api_version`` is ``v2alpha``;
   ``outcome`` is ``parsed`` when the ID parses as an integer, otherwise ``parse_error``.
   This records reconnect attempts, not whether the client successfully received replayed events.

``etos_sse_retries_total{api_version,outcome,etos_version}``
   Counts response-write outcomes for the retryable ``error`` event emitted when the underlying
   stream closes. ``outcome`` is ``write_success`` or ``write_error``. Other stream errors do not
   request a client retry and are not counted here.

``etos_sse_disconnects_total{api_version,outcome,etos_version}``
   Counts active streams ended by request-context cancellation before a terminal result, or by
   a response write error. ``outcome`` is ``request_context_cancelled`` or ``write_error``.

``etos_sse_event_delivery_total{api_version,event,outcome,etos_version}``
   Counts each valid event frame at the response boundary. ``event`` is one of ``artifact``,
   ``error``, ``message``, ``ping``, ``report``, ``shutdown``, ``status``, or ``unknown``;
   ``outcome`` is ``write_success`` or ``write_error``. ``write_success`` means the frame write
   returned without error and the server called ``Flush``; SSE has no client acknowledgement, so
   it does not prove the client received the frame.

``etos_sse_events_dropped_total{api_version,event,outcome,etos_version}``
   Counts events dropped before delivery because JSON parsing failed (``parse_error``) or the
   event did not match the v2alpha schema (``schema_invalid``). The ``event`` label is normalized
   to the bounded event vocabulary above.

``etos_sse_terminal_results_total{api_version,outcome,etos_version}``
   Counts terminal ``shutdown`` result handling. ``outcome`` is ``write_success`` when the event
   frame was written and flushed, or ``write_error`` when the response write failed. A shutdown
   event rejected by protocol validation is not terminal: the stream stays open and the event is
   counted only in ``etos_sse_events_dropped_total``. Success denotes the SSE server-side write
   boundary, not confirmed client receipt.

All known SSE label combinations are initialized to zero, making no activity distinguishable
from a missing metric series. SSE labels contain only the API version, normalized event type,
outcome, and ETOS release version; they never contain TestRun IDs, event IDs, URLs, or raw errors.


Contribute
==========

- Issue Tracker: https://github.com/eiffel-community/etos/issues
- Source Code: https://github.com/eiffel-community/etos-api


Support
=======

If you are having issues, please let us know.
There is a mailing list at: etos-maintainers@googlegroups.com or just write an Issue.

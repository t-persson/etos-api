FROM python:3.15-trixie AS build

COPY .git /src/.git
COPY .gitignore /src/.gitignore
COPY python /src/python
WORKDIR /src/python
RUN pip install --no-cache-dir build==1.3.0 && python3 -m build

FROM python:3.15-slim-trixie
ARG TZ
ENV TZ=$TZ

ARG BUILD_REVISION
RUN test -n "$BUILD_REVISION" || (echo "BUILD_REVISION must be supplied" >&2; exit 1)
ENV BUILD_REVISION=$BUILD_REVISION

COPY --from=build /src/python/dist/*.whl /tmp/

# hadolint ignore=DL3013
# hadolint ignore=DL3008
RUN apt-get update && \
    apt-get install -y gcc libc-dev tzdata --no-install-recommends && \
    pip install --no-cache-dir /tmp/*.whl && \
    apt-get purge -y --auto-remove gcc libc-dev && \
    rm -rf /var/lib/apt/lists/*

RUN groupadd -r etos && useradd -r -m -s /bin/false -g etos etos
USER etos
EXPOSE 8080

LABEL org.opencontainers.image.source=https://github.com/eiffel-community/etos-api
LABEL org.opencontainers.image.authors=etos-maintainers@googlegroups.com
LABEL org.opencontainers.image.licenses=Apache-2.0

ENTRYPOINT ["uvicorn", "etos_api.main:APP", "--host=0.0.0.0", "--port=8080"]

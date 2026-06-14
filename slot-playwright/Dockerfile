# syntax=docker/dockerfile:1.7

# Dedicated browser server for leased Glimmung test slots. The Playwright npm
# package version must match every protocol client that connects over the
# WebSocket: mcp-glimmung's browser_inspector.mjs and the run sidecar's capture
# tools (the runner image's pinned `playwright`). Playwright rejects clients on
# a different major/minor, so all of them move together or not at all.
FROM mcr.microsoft.com/playwright:v1.60.0-noble

ARG PLAYWRIGHT_VERSION=1.60.0

ENV PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
RUN npm install -g "playwright@${PLAYWRIGHT_VERSION}"

ENV NODE_PATH=/usr/lib/node_modules
ENV PLAYWRIGHT_PACKAGE_PATH=/usr/lib/node_modules/playwright/index.mjs
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright

EXPOSE 3000

CMD ["npx", "playwright", "run-server", "--host", "0.0.0.0", "--port", "3000"]

FROM node:24-bookworm-slim AS sharp-build
WORKDIR /processor/sharp
COPY image-processor/sharp/package*.json ./
RUN npm ci --omit=dev
COPY image-processor/sharp/upscale.mjs ./

FROM python:3.12-slim-bookworm
COPY --from=sharp-build /usr/local/bin/node /usr/local/bin/node
COPY --from=sharp-build /processor/sharp /processor/sharp
COPY image-processor/requirements.txt /tmp/requirements.txt
RUN apt-get update && apt-get install -y --no-install-recommends libstdc++6 libgcc-s1 && rm -rf /var/lib/apt/lists/* && pip install --no-cache-dir -r /tmp/requirements.txt && useradd --uid 10001 --create-home app
COPY image-processor/server.py /processor/server.py
COPY image-processor/models /processor/models
USER app
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 IMAGE_PROCESSOR_CONCURRENCY=2
WORKDIR /processor
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/health',timeout=2)"
CMD ["python", "server.py"]

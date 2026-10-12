FROM python:3.12-slim-bookworm
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 AGNO_TELEMETRY=false
WORKDIR /app
COPY runtime/agno/requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r requirements.txt
# Keep the model SDK layer reusable when MCP dependencies change.
COPY runtime/agno/mcp-requirements.txt /app/mcp-requirements.txt
RUN pip install --no-cache-dir -r mcp-requirements.txt
COPY runtime/agno/runtime.py runtime/agno/service.py runtime/agno/mcp_runtime.py /app/
# Compose overrides this with the host control service UID/GID.
USER 65532:65532
ENTRYPOINT ["python", "-I", "-u", "/app/service.py"]

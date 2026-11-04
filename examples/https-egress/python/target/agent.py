"""A normal tracing + guardrail application, with no Interceptor imports."""
import json
import logging
import os
import socket
import sys
import urllib.request
from langfuse import Langfuse, get_client

class Agent:
    def __init__(self):
        print("application initialized", file=sys.stderr)
        self.tracing = get_client()
        with self.tracing.start_as_current_observation(name="https-seam-startup"):
            pass
        self.tracing.flush()  # Background export before any campaign turn.
        try:
            socket.getaddrinfo("undeclared.invalid", 443)
        except socket.gaierror:
            self.undeclared_blocked = True
        else:
            raise RuntimeError("undeclared DNS escaped sandbox")

    def invoke(self, value):
        message = value["message"]
        if message == "sdk-error":
            try:
                Langfuse(sample_rate=2.0)
            except ValueError:
                logging.exception("Langfuse initialization failed before a request")
                raise
        with self.tracing.start_as_current_observation(name="https-seam-guardrail"):
            request = urllib.request.Request(os.environ["GUARDRAIL_BASE_URL"] + "/check",
                data=json.dumps({"text": message, "diagnostic": "request-sentinel"}).encode(),
                headers={"Content-Type": "application/json", "Authorization": "Bearer synthetic-placeholder"})
            with urllib.request.urlopen(request, timeout=10) as response:
                verdict = json.load(response)
        self.tracing.flush()
        return {"executed": verdict["allowed"], "undeclared_blocked": self.undeclared_blocked}

    def close(self):
        with self.tracing.start_as_current_observation(name="https-seam-shutdown"):
            pass
        self.tracing.flush()
        self.tracing.shutdown()
        print("application closed", file=sys.stderr)

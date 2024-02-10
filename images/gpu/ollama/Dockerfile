# https://hub.docker.com/r/ollama/ollama
FROM ollama/ollama:0.1.24

ENV PATH=$PATH:/usr/local/nvidia/bin:/bin/nvidia/bin
ENV OLLAMA_ORIGINS=*
ENV OLLAMA_HOST=0.0.0.0:11434

COPY preload_model.sh /tmp/
RUN chmod 555 /tmp/preload_model.sh

# Pre-install a few models.
# Although these are the smallest possible model size (7B parameters),
# these are still quite large and it would take too long for tests to
# download them on every run.
RUN /tmp/preload_model.sh     \
      llava:7b-v1.6           \
      codellama:7b-instruct   \
      llama2-chinese:7b-chat

RUN rm -f /tmp/preload_model.sh

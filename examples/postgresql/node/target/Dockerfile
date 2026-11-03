FROM node:22-bookworm-slim
WORKDIR /app
RUN npm install --omit=dev --save-exact pg@8.16.3
COPY support_app.mjs /app/support_app.mjs

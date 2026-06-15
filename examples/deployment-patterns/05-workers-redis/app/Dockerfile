FROM node:22-alpine
WORKDIR /app
COPY package.json ./
RUN npm install --omit=dev
COPY api.js worker.js ./
EXPOSE 3000
CMD ["node", "api.js"]

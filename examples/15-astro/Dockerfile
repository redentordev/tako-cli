FROM node:20-alpine AS builder

WORKDIR /app

COPY package*.json ./
RUN npm ci

COPY . .
RUN npm run build

FROM node:20-alpine

WORKDIR /app

COPY --from=builder /app/dist dist/
COPY --from=builder /app/package.json .
COPY --from=builder /app/package-lock.json .

ENV NODE_ENV=production
ENV HOST=0.0.0.0
ENV PORT=3000

# Install production dependencies for SSR
RUN npm ci --omit=dev

EXPOSE 3000

CMD ["node", "./dist/server/entry.mjs"]

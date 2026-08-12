# Atlas UI — production image.
#
# Multi-stage: the Vite build needs Node and node_modules, neither of which
# belong in the image that actually serves traffic. The runtime stage is
# nginx serving the static bundle, with the same same-origin API/WebSocket
# proxying vite.config.ts's dev server already does — see nginx.conf.

# ----------------------------------------------------------------- build ---
FROM node:22-alpine AS build

WORKDIR /src
# Dependencies in their own layer, so a source-only change does not
# reinstall node_modules.
COPY package.json package-lock.json ./
RUN npm ci

COPY . .
RUN npm run build

# --------------------------------------------------------------- runtime ---
FROM nginx:1.27-alpine

COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /src/dist /usr/share/nginx/html

EXPOSE 80

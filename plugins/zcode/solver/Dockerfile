FROM node:20-bookworm-slim AS build
WORKDIR /solver
COPY package.json ./
RUN npm install --ignore-scripts
COPY *.ts ./
RUN npx esbuild solver-server.mts --bundle --platform=node --format=cjs --outfile=dist/solver-server.cjs

FROM node:20-bookworm-slim
WORKDIR /solver
COPY --from=build /solver/dist/solver-server.cjs ./dist/solver-server.cjs
USER node
EXPOSE 8777
CMD ["node", "dist/solver-server.cjs"]

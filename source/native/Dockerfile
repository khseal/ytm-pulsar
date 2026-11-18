FROM debian:buster-slim
RUN printf 'deb http://archive.debian.org/debian buster main\ndeb http://archive.debian.org/debian-security buster/updates main\n' > /etc/apt/sources.list \
 && apt-get -o Acquire::Check-Valid-Until=false update \
 && apt-get install -y --no-install-recommends gcc-arm-linux-gnueabihf libc6-dev-armhf-cross python3
WORKDIR /src
COPY resolver.c ./
COPY .build/quickjs-amalgam.c .build/quickjs.h ./
RUN arm-linux-gnueabihf-gcc -O2 -static -D_GNU_SOURCE resolver.c quickjs-amalgam.c -lm -ldl -pthread -latomic -s -o /native-resolver
